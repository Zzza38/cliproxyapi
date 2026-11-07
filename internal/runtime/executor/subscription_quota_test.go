package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestSubscriptionQuotaUsesExecutorTransport(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			reset := time.Now().Add(24 * time.Hour)
			requests := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer test-access-token" {
					t.Errorf("request = %s %s, bearer credentials missing", r.Method, r.URL.Path)
				}
				switch r.URL.Path {
				case "/wham/usage":
					if r.Header.Get("ChatGPT-Account-Id") != "test-account" {
						t.Error("account ID missing")
					}
					fmt.Fprintf(w, `{"plan_type":"prolite","rate_limit":{"primary_window":{"used_percent":20,"limit_window_seconds":604800,"reset_at":%d}}}`, reset.Unix())
				case "/api/oauth/usage":
					if !strings.Contains(r.Header.Get("Anthropic-Beta"), "oauth") {
						t.Error("OAuth beta header missing")
					}
					fmt.Fprintf(w, `{"seven_day":{"utilization":20,"resets_at":%q}}`, reset.Format(time.RFC3339))
				case "/api/oauth/profile":
					fmt.Fprint(w, `{"organization":{"rate_limit_tier":"default_claude_max_20x"}}`)
				default:
					t.Errorf("unexpected URL %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()
			auth := &coreauth.Auth{Provider: provider, Metadata: map[string]any{"access_token": "test-access-token", "account_id": "test-account"}, Attributes: map[string]string{"base_url": upstream.URL}}
			var executor coreauth.SubscriptionQuotaProvider
			if provider == "codex" {
				auth.Attributes["base_url"] += "/codex"
				executor = NewCodexAutoExecutor(&config.Config{})
			} else {
				executor = NewClaudeExecutor(&config.Config{})
			}
			quota, err := executor.FetchSubscriptionQuota(context.Background(), auth)
			if err != nil {
				t.Fatal(err)
			}
			if !quota.WeeklyKnown || quota.WeeklyRemaining != .8 || quota.CapacityMultiplier != 10 {
				t.Fatalf("quota=%+v", quota)
			}
			wantRequests := 1
			if provider == "claude" {
				wantRequests = 2
			}
			if requests != wantRequests {
				t.Fatalf("requests=%d; want %d", requests, wantRequests)
			}
		})
	}
}
