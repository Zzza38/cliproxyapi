package auth

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// PooledQuotaHeaders reports observed quota across accounts that can serve this
// route. Each account contributes one equal-sized allowance: upstream percentages
// do not describe absolute plan capacities. Unknown windows are omitted rather
// than treating an unobserved account as full or exhausted.
func (m *Manager) PooledQuotaHeaders(ctx context.Context, providers []string, model string, opts coreexecutor.Options, current http.Header) http.Header {
	return m.pooledQuotaHeaders(ctx, providers, model, opts, current, time.Now())
}

func (m *Manager) pooledQuotaHeaders(ctx context.Context, providers []string, model string, opts coreexecutor.Options, current http.Header, now time.Time) http.Header {
	out := make(http.Header)
	if m == nil || m.HomeEnabled() {
		return out
	}
	eligible := authSelectionEligibilityForRequest(ctx, opts)
	selected, _ := opts.Metadata[coreexecutor.SelectedAuthMetadataKey].(string)
	providerSet := make(map[string]bool, len(providers))
	for _, provider := range providers {
		providerSet[strings.ToLower(strings.TrimSpace(provider))] = true
	}
	pools := map[string]map[string]QuotaState{}
	registryRef := registry.GetGlobalRegistry()
	m.mu.RLock()
	for _, a := range m.auths {
		if a == nil || a.Disabled || a.Status == StatusDisabled || !eligible.allows(a) {
			continue
		}
		provider := strings.ToLower(strings.TrimSpace(a.Provider))
		if (provider != "codex" && provider != "claude") || !providerSet[provider] {
			continue
		}
		if model != "" && !m.authSupportsRouteModel(registryRef, a, model) {
			continue
		}
		q := a.Quota.Clone()
		if a.ID == selected {
			q.ObserveResponseHeadersForProvider(provider, current, now)
		}
		if pools[provider] == nil {
			pools[provider] = make(map[string]QuotaState)
		}
		key := quotaAccountKey(a)
		if previous, ok := pools[provider][key]; !ok || q.ObservedAt.After(previous.ObservedAt) {
			pools[provider][key] = q
		}
	}
	m.mu.RUnlock()
	for provider, accounts := range pools {
		if provider == "codex" {
			for _, window := range []string{"primary", "secondary"} {
				prefix := "X-Codex-" + window + "-"
				aggregateQuotaWindow(out, accounts, prefix+"Used-Percent", prefix+"Reset-At", prefix+"Window-Minutes", 100, now)
			}
		} else {
			for _, window := range []string{"5h", "7d", "7d-sonnet", "7d-opus", "7d_oi"} {
				prefix := "Anthropic-Ratelimit-Unified-" + window + "-"
				aggregateQuotaWindow(out, accounts, prefix+"Utilization", prefix+"Reset", "", 1, now)
			}
			if value := out.Get("Anthropic-Ratelimit-Unified-5h-Utilization"); value != "" {
				status := "allowed"
				for key := range out {
					if strings.HasSuffix(strings.ToLower(key), "-utilization") && out.Get(key) == "1" {
						status = "rejected"
					}
				}
				out.Set("Anthropic-Ratelimit-Unified-Status", status)
			}
		}
	}
	return out
}

func quotaAccountKey(a *Auth) string {
	keys := []string{"account_id"}
	if strings.EqualFold(a.Provider, "claude") {
		keys = []string{"organization_uuid", "account_uuid"}
	}
	var parts []string
	for _, key := range keys {
		value := authMetadataString(a, key)
		if value == "" {
			value = strings.TrimSpace(a.Attributes[key])
		}
		parts = append(parts, value)
	}
	// Claude organization alone does not identify an individual allowance.
	if parts[len(parts)-1] != "" {
		return strings.Join(parts, "\x00")
	}
	return "id:" + a.ID
}

func aggregateQuotaWindow(out http.Header, accounts map[string]QuotaState, usedKey, resetKey, durationKey string, scale float64, now time.Time) {
	var sum float64
	var reset int64
	var duration string
	for _, q := range accounts {
		value, err := strconv.ParseFloat(q.Signals[http.CanonicalHeaderKey(usedKey)], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || q.ObservedAt.IsZero() {
			return
		}
		// An expired observation does not establish how much was used after reset.
		deadline, errReset := strconv.ParseInt(q.Signals[http.CanonicalHeaderKey(resetKey)], 10, 64)
		if errReset != nil && scale == 100 {
			seconds, errSeconds := strconv.ParseInt(q.Signals[http.CanonicalHeaderKey(strings.TrimSuffix(resetKey, "Reset-At")+"Reset-After-Seconds")], 10, 64)
			if errSeconds == nil && seconds > 0 && seconds <= 31536000 {
				deadline = q.ObservedAt.Unix() + seconds
				errReset = nil
			}
		}
		if errReset != nil || deadline <= now.Unix() {
			return
		}
		if reset == 0 || deadline < reset {
			reset = deadline
		}
		if durationKey != "" {
			minutes := q.Signals[http.CanonicalHeaderKey(durationKey)]
			parsed, errDuration := strconv.ParseInt(minutes, 10, 64)
			if errDuration != nil || parsed <= 0 || (duration != "" && duration != minutes) {
				return
			}
			duration = minutes
		}
		sum += math.Min(value, scale)
	}
	if len(accounts) == 0 {
		return
	}
	average := math.Round(sum/float64(len(accounts))*1e6) / 1e6
	out.Set(usedKey, strconv.FormatFloat(average, 'f', -1, 64))
	out.Set(resetKey, strconv.FormatInt(reset, 10))
	if durationKey != "" {
		out.Set(durationKey, duration)
	}
}
