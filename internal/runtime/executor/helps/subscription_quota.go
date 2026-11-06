package helps

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type SubscriptionQuotaRequest func(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error)

// FetchSubscriptionQuota uses the provider's authenticated transport for read-only
// usage probes. It never invokes the endpoint that redeems a reset credit.
func FetchSubscriptionQuota(ctx context.Context, auth *coreauth.Auth, provider, baseURL string, send SubscriptionQuotaRequest) (*coreauth.SubscriptionQuota, error) {
	observedAt := time.Now()
	get := func(path string) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if provider == "codex" {
			if account, ok := auth.Metadata["account_id"].(string); ok {
				req.Header.Set("ChatGPT-Account-Id", account)
			}
		} else {
			req.Header.Set("anthropic-beta", "oauth-2025-04-20")
		}
		resp, err := send(ctx, auth, req)
		if err != nil {
			return nil, err
		}
		defer func() {
			if errClose := resp.Body.Close(); errClose != nil {
				log.WithError(errClose).Debug("close subscription quota response")
			}
		}()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("subscription quota: HTTP %d", resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	}
	path := "/api/oauth/usage"
	if provider == "codex" {
		path = "/wham/usage"
	}
	body, err := get(path)
	if err != nil {
		return nil, err
	}
	if !gjson.ValidBytes(body) {
		return nil, fmt.Errorf("subscription quota: invalid JSON")
	}
	now := observedAt
	quota := ParseSubscriptionQuota(provider, body, auth, now)
	if provider == "claude" {
		if profile, errProfile := get("/api/oauth/profile"); errProfile == nil {
			if tier := gjson.GetBytes(profile, "organization.rate_limit_tier").String(); tier != "" {
				quota.CapacityMultiplier = SubscriptionCapacity(provider, tier, auth)
			}
		}
	}
	if provider == "codex" && gjson.GetBytes(body, "rate_limit_reset_credits.available_count").Int() > 0 {
		resets, errResets := get("/wham/rate-limit-reset-credits")
		if errResets == nil {
			quota.ManualResetExpiries = ParseManualResetExpiries(resets, now)
		}
	}
	return quota, nil
}

// ParseSubscriptionQuota normalizes weekly limits without assuming that Codex's
// primary window is always the short window; some plans expose only a weekly one.
func ParseSubscriptionQuota(provider string, body []byte, auth *coreauth.Auth, now time.Time) *coreauth.SubscriptionQuota {
	q := &coreauth.SubscriptionQuota{ObservedAt: now, CapacityMultiplier: SubscriptionCapacity(provider, gjson.GetBytes(body, "plan_type").String(), auth)}
	if provider == "claude" {
		weekly := gjson.GetBytes(body, "seven_day")
		setSubscriptionWindow(q, weekly.Get("utilization"), parseSubscriptionTime(weekly.Get("resets_at")), true, now)
		short := gjson.GetBytes(body, "five_hour")
		setSubscriptionWindow(q, short.Get("utilization"), parseSubscriptionTime(short.Get("resets_at")), false, now)
		return q
	}
	for _, name := range []string{"primary_window", "secondary_window"} {
		window := gjson.GetBytes(body, "rate_limit."+name)
		seconds := window.Get("limit_window_seconds").Int()
		if seconds == 0 {
			seconds = window.Get("window_minutes").Int() * 60
		}
		reset := parseSubscriptionTime(window.Get("reset_at"))
		if reset.IsZero() && window.Get("reset_after_seconds").Exists() {
			reset = now.Add(time.Duration(window.Get("reset_after_seconds").Int()) * time.Second)
		}
		setSubscriptionWindow(q, window.Get("used_percent"), reset, seconds == 7*24*60*60, now)
	}
	// Backend admission flags can reject usage before a rounded percentage
	// reaches 100. Keep that account out until a new probe or snapshot expiry.
	if q.BlockedUntil.IsZero() && (gjson.GetBytes(body, "rate_limit.allowed").Type == gjson.False || gjson.GetBytes(body, "rate_limit.limit_reached").Type == gjson.True) {
		q.BlockedUntil = now.Add(5 * time.Minute)
	}
	return q
}

func setSubscriptionWindow(q *coreauth.SubscriptionQuota, used gjson.Result, reset time.Time, weekly bool, now time.Time) {
	if !used.Exists() || used.Type != gjson.Number || !reset.After(now) {
		return
	}
	value := used.Float()
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return
	}
	if weekly {
		q.WeeklyKnown = true
		q.WeeklyResetAt = reset
		q.WeeklyRemaining = math.Max(0, 1-value/100)
	}
	if value >= 100 && reset.After(q.BlockedUntil) {
		q.BlockedUntil = reset
	}
}

func parseSubscriptionTime(value gjson.Result) time.Time {
	if value.Type == gjson.Number {
		return time.Unix(value.Int(), 0)
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value.String()); err == nil {
		return parsed
	}
	if seconds, err := strconv.ParseInt(value.String(), 10, 64); err == nil {
		return time.Unix(seconds, 0)
	}
	return time.Time{}
}

// ParseManualResetExpiries ignores spent credits, credits without deadlines,
// and deadlines that have already passed.
func ParseManualResetExpiries(body []byte, now time.Time) []time.Time {
	var expiries []time.Time
	for _, credit := range gjson.GetBytes(body, "credits").Array() {
		if !strings.EqualFold(credit.Get("status").String(), "available") {
			continue
		}
		expiry := parseSubscriptionTime(credit.Get("expires_at"))
		if expiry.After(now) {
			expiries = append(expiries, expiry)
		}
	}
	return expiries
}

// SubscriptionCapacity implements the operator's base-plan accounting policy.
// Unknown tiers use one unit rather than guessing a paid entitlement.
func SubscriptionCapacity(provider, plan string, auth *coreauth.Auth) float64 {
	if plan == "" && auth != nil {
		plan = auth.Attributes["plan_type"]
		if plan == "" {
			plan, _ = auth.Metadata["plan_type"].(string)
		}
		if provider == "claude" {
			if tier, ok := auth.Metadata["rate_limit_tier"].(string); ok {
				plan = tier
			}
		}
	}
	plan = strings.NewReplacer(" ", "", "_", "", "-", "").Replace(strings.ToLower(plan))
	if provider == "codex" {
		switch plan {
		case "pro100", "prolite":
			return 10
		case "pro", "pro200":
			return 20
		case "promax", "pro500":
			return 50
		}
	} else if provider == "claude" {
		switch plan {
		case "max5x", "defaultclaudemax5x":
			return 5
		case "max20x", "defaultclaudemax20x", "max10x":
			return 10
		}
	}
	return 1
}
