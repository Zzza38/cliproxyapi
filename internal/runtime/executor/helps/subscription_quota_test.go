package helps

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSubscriptionWeeklyWindows(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, tc := range []struct {
		name, provider, payload string
		remaining, capacity     float64
	}{
		{"codex primary weekly", "codex", `{"plan_type":"pro_100","rate_limit":{"primary_window":{"used_percent":25,"limit_window_seconds":604800,"reset_at":1800600000}}}`, .75, 10},
		{"codex secondary weekly", "codex", `{"plan_type":"promax","rate_limit":{"primary_window":{"used_percent":99,"limit_window_seconds":18000,"reset_at":1800010000},"secondary_window":{"used_percent":50,"limit_window_seconds":604800,"reset_at":1800600000}}}`, .5, 50},
		{"claude weekly", "claude", `{"seven_day":{"utilization":30,"resets_at":"2027-01-21T14:40:00Z"},"five_hour":{"utilization":5,"resets_at":"2027-01-15T12:00:00Z"}}`, .7, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			quota := ParseSubscriptionQuota(tc.provider, []byte(tc.payload), &coreauth.Auth{}, now)
			if !quota.WeeklyKnown || quota.WeeklyRemaining != tc.remaining || quota.CapacityMultiplier != tc.capacity {
				t.Fatalf("unexpected quota: %+v", quota)
			}
		})
	}
}

func TestSubscriptionQuotaExhaustionAndMalformedWindows(t *testing.T) {
	now := time.Unix(1800000000, 0)
	q := ParseSubscriptionQuota("codex", []byte(`{"rate_limit":{"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_at":1800010000},"secondary_window":{"used_percent":50,"limit_window_seconds":604800,"reset_at":1800600000}}}`), nil, now)
	if !q.BlockedUntil.Equal(time.Unix(1800010000, 0)) || !q.WeeklyKnown {
		t.Fatalf("quota = %+v", q)
	}
	denied := ParseSubscriptionQuota("codex", []byte(`{"rate_limit":{"allowed":false,"primary_window":{"used_percent":99,"limit_window_seconds":604800,"reset_at":1800600000}}}`), nil, now)
	if !denied.BlockedUntil.After(now) {
		t.Fatal("backend denial was ignored because utilization rounded below 100")
	}
	overshot := ParseSubscriptionQuota("codex", []byte(`{"rate_limit":{"primary_window":{"used_percent":101,"limit_window_seconds":604800,"reset_at":1800600000}}}`), nil, now)
	if !overshot.WeeklyKnown || overshot.WeeklyRemaining != 0 || !overshot.BlockedUntil.After(now) {
		t.Fatal("overshot quota must be exhausted")
	}
	for _, used := range []string{`null`, `"50"`, `-1`} {
		q := ParseSubscriptionQuota("codex", []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%s,"limit_window_seconds":604800,"reset_at":1800600000}}}`, used)), nil, now)
		if q.WeeklyKnown || !q.BlockedUntil.IsZero() {
			t.Fatalf("malformed usage %s accepted: %+v", used, q)
		}
	}
}

func TestSubscriptionCapacityPolicy(t *testing.T) {
	for _, tc := range []struct {
		provider, plan string
		want           float64
	}{
		{"codex", "plus", 1}, {"codex", "Pro 100", 10}, {"codex", "prolite", 10}, {"codex", "pro_200", 20}, {"codex", "Pro 500", 50}, {"codex", "promax", 50},
		{"claude", "pro", 1}, {"claude", "default_claude_max_5x", 5}, {"claude", "default_claude_max_20x", 10}, {"codex", "unknown", 1},
	} {
		if got := SubscriptionCapacity(tc.provider, tc.plan, nil); got != tc.want {
			t.Errorf("%s/%s = %v; want %v", tc.provider, tc.plan, got, tc.want)
		}
	}
}

func TestSubscriptionQuotaReadOnlyFetch(t *testing.T) {
	now := time.Now()
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method != http.MethodGet {
			t.Errorf("unexpected mutation: %s", r.Method)
		}
		if r.Header.Get("ChatGPT-Account-Id") != "account-a" {
			t.Error("missing account identity")
		}
		switch r.URL.Path {
		case "/wham/usage":
			fmt.Fprintf(w, `{"plan_type":"pro_100","rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_at":%d}},"rate_limit_reset_credits":{"available_count":3}}`, now.Add(5*24*time.Hour).Unix())
		case "/wham/rate-limit-reset-credits":
			fmt.Fprintf(w, `{"credits":[{"status":"available","expires_at":%q},{"status":"consumed","expires_at":%q},{"status":"available","expires_at":%q},{"status":"available","expires_at":null}]}`, now.Add(time.Hour).Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339))
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	auth := &coreauth.Auth{Metadata: map[string]any{"account_id": "account-a"}}
	quota, err := FetchSubscriptionQuota(context.Background(), auth, "codex", server.URL, func(ctx context.Context, a *coreauth.Auth, req *http.Request) (*http.Response, error) {
		return server.Client().Do(req)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || len(quota.ManualResetExpiries) != 1 || !quota.WeeklyKnown || quota.CapacityMultiplier != 10 {
		t.Fatalf("calls=%v quota=%+v", calls, quota)
	}
}

func TestClaudeProfileIsReusedBetweenQuotaReads(t *testing.T) {
	calls := 0
	now := time.Now()
	auth := &coreauth.Auth{Quota: coreauth.QuotaState{Subscription: &coreauth.SubscriptionQuota{ProfileCheckedAt: now, CapacityMultiplier: 10}}}
	q, err := FetchSubscriptionQuota(context.Background(), auth, "claude", "https://example.test", func(_ context.Context, _ *coreauth.Auth, r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/api/oauth/usage" {
			t.Fatalf("unnecessary profile fetch: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"seven_day":{"utilization":20,"resets_at":"2099-01-01T00:00:00Z"}}`))}, nil
	})
	if err != nil || calls != 1 || q.CapacityMultiplier != 10 || !q.ProfileCheckedAt.Equal(now) {
		t.Fatalf("quota=%+v calls=%d err=%v", q, calls, err)
	}
}

func TestSubscriptionRetryAfterParsing(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		raw  string
		want time.Duration
	}{{"0", 0}, {"-1", 0}, {"nonsense", 0}, {"600", 10 * time.Minute}, {now.Add(20 * time.Minute).Format(http.TimeFormat), 20 * time.Minute}} {
		if got := subscriptionRetryAfter(tc.raw, now); got != tc.want {
			t.Errorf("%q: %v != %v", tc.raw, got, tc.want)
		}
	}
}

func TestClaudeReadOnlyQuotaRetainsPoolWindows(t *testing.T) {
	now := time.Unix(1800000000, 0)
	q := ParseSubscriptionQuota("claude", []byte(`{"five_hour":null,"seven_day":{"utilization":75,"resets_at":"2027-01-21T14:40:00Z"},"extra_usage":{"utilization":100},"cedar_ember":{"available_count":3}}`), &coreauth.Auth{}, now)
	if got := q.UsageSignals["Anthropic-Ratelimit-Unified-7d-Utilization"]; got != "0.75" {
		t.Fatalf("weekly usage = %q", got)
	}
	if got := q.UsageSignals["Anthropic-Ratelimit-Unified-7d-Reset"]; got != "1800542400" {
		t.Fatalf("weekly reset = %q", got)
	}
	if len(q.UsageSignals) != 2 {
		t.Fatalf("unknown session, spend or credits leaked into pool: %v", q.UsageSignals)
	}
	for _, payload := range []string{
		`{"seven_day":{"utilization":"75","resets_at":"2027-01-21T14:40:00Z"}}`,
		`{"seven_day":{"utilization":-1,"resets_at":"2027-01-21T14:40:00Z"}}`,
		`{"seven_day":{"utilization":75,"resets_at":"2020-01-01T00:00:00Z"}}`,
	} {
		bad := ParseSubscriptionQuota("claude", []byte(payload), &coreauth.Auth{}, now)
		if len(bad.UsageSignals) != 0 {
			t.Fatalf("invalid quota accepted: %v", bad.UsageSignals)
		}
	}
}
