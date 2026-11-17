package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func poolTestQuota(provider, used string, now time.Time) QuotaState {
	h := make(http.Header)
	if provider == "codex" {
		h.Set("X-Codex-Primary-Used-Percent", used)
		h.Set("X-Codex-Primary-Window-Minutes", "300")
		h.Set("X-Codex-Primary-Reset-At", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	} else {
		h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", used)
		h.Set("Anthropic-Ratelimit-Unified-5h-Reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	}
	var q QuotaState
	q.ObserveResponseHeadersForProvider(provider, h, now)
	return q
}

func TestPooledQuotaHeaders(t *testing.T) {
	now := time.Unix(1800000000, 0)
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			first, second, want := "80", "20", "50"
			key := "X-Codex-Primary-Used-Percent"
			if provider == "claude" {
				first, second, want = "0.8", "0.2", "0.5"
				key = "Anthropic-Ratelimit-Unified-5h-Utilization"
			}
			a := &Auth{ID: "a", Provider: provider, Quota: poolTestQuota(provider, first, now)}
			b := &Auth{ID: "b", Provider: provider, Quota: poolTestQuota(provider, second, now)}
			m := NewManager(nil, nil, nil)
			m.auths = map[string]*Auth{"a": a, "b": b}
			headers := m.pooledQuotaHeaders(context.Background(), []string{provider}, "", coreexecutor.Options{}, nil, now)
			if got := headers.Get(key); got != want {
				t.Fatalf("pooled usage = %q, want %q", got, want)
			}
			if provider == "claude" && headers.Get("Anthropic-Ratelimit-Unified-Status") != "allowed" {
				t.Fatal(headers)
			}
			for _, test := range []struct {
				name  string
				alter func(*Auth)
			}{
				{"unknown", func(a *Auth) { a.Quota = QuotaState{} }},
				{"expired", func(a *Auth) { a.Quota = poolTestQuota(provider, first, now.Add(-2*time.Hour)) }},
				{"nan", func(a *Auth) { a.Quota.Signals[http.CanonicalHeaderKey(key)] = "NaN" }},
			} {
				t.Run(test.name, func(t *testing.T) {
					original := a.Quota.Clone()
					test.alter(a)
					defer func() { a.Quota = original }()
					h := m.pooledQuotaHeaders(context.Background(), []string{provider}, "", coreexecutor.Options{}, nil, now)
					if h.Get(key) != "" {
						t.Fatalf("incomplete pool reported: %v", h)
					}
				})
			}
			a.Disabled = true
			if got := m.pooledQuotaHeaders(context.Background(), []string{provider}, "", coreexecutor.Options{}, nil, now).Get(key); got != second {
				t.Fatalf("disabled account counted: %s", got)
			}
		})
	}
}

func TestPooledQuotaDeduplicatesAndUsesCurrentResponse(t *testing.T) {
	now := time.Unix(1800000000, 0)
	m := NewManager(nil, nil, nil)
	a := &Auth{ID: "a", Provider: "codex", Metadata: map[string]any{"account_id": "same"}, Quota: poolTestQuota("codex", "80", now.Add(-time.Minute))}
	b := &Auth{ID: "b", Provider: "codex", Metadata: map[string]any{"account_id": "same"}, Quota: poolTestQuota("codex", "20", now)}
	c := &Auth{ID: "c", Provider: "codex", Quota: poolTestQuota("codex", "40", now)}
	m.auths = map[string]*Auth{"a": a, "b": b, "c": c}
	opts := coreexecutor.Options{Metadata: map[string]any{coreexecutor.SelectedAuthMetadataKey: "a"}}
	current := make(http.Header)
	for k, v := range poolTestQuota("codex", "60", now).Signals {
		current.Set(k, v)
	}
	h := m.pooledQuotaHeaders(context.Background(), []string{"codex"}, "", opts, current, now.Add(time.Second))
	if got := h.Get("X-Codex-Primary-Used-Percent"); got != "50" {
		t.Fatalf("dedup/current = %s, want 50", got)
	}
	if a.Quota.Signals["X-Codex-Primary-Used-Percent"] != "80" {
		t.Fatal("mutated stored observation")
	}
}

func TestPooledQuotaRejectsDifferentWindowSizes(t *testing.T) {
	now := time.Unix(1800000000, 0)
	m := NewManager(nil, nil, nil)
	a := &Auth{ID: "a", Provider: "codex", Quota: poolTestQuota("codex", "80", now)}
	b := &Auth{ID: "b", Provider: "codex", Quota: poolTestQuota("codex", "20", now)}
	b.Quota.Signals["X-Codex-Primary-Window-Minutes"] = "10080"
	m.auths = map[string]*Auth{"a": a, "b": b}
	if h := m.pooledQuotaHeaders(context.Background(), []string{"codex"}, "", coreexecutor.Options{}, nil, now); len(h) != 0 {
		t.Fatalf("incompatible windows combined: %v", h)
	}
}

func TestPooledQuotaRelativeResetAndProviderIsolation(t *testing.T) {
	now := time.Unix(1800000000, 0)
	a := &Auth{ID: "a", Provider: "codex", Quota: poolTestQuota("codex", "30", now)}
	delete(a.Quota.Signals, "X-Codex-Primary-Reset-At")
	a.Quota.Signals["X-Codex-Primary-Reset-After-Seconds"] = "120"
	m := NewManager(nil, nil, nil)
	m.auths = map[string]*Auth{"a": a, "other": {ID: "other", Provider: "claude"}}
	h := m.pooledQuotaHeaders(context.Background(), []string{"codex"}, "", coreexecutor.Options{}, nil, now.Add(time.Minute))
	if got := h.Get("X-Codex-Primary-Reset-At"); got != strconv.FormatInt(now.Unix()+120, 10) {
		t.Fatalf("reset = %s", got)
	}
	if len(h) != 3 {
		t.Fatalf("unexpected provider/window headers: %v", h)
	}
	h = m.pooledQuotaHeaders(context.Background(), []string{"codex"}, "", coreexecutor.Options{}, nil, now.Add(3*time.Minute))
	if len(h) != 0 {
		t.Fatalf("expired relative reset reported: %v", h)
	}
}

func TestPooledQuotaUsesOnlyAccountsSupportingModel(t *testing.T) {
	now := time.Unix(1800000000, 0)
	m := NewManager(nil, nil, nil)
	a := &Auth{ID: "pool-supported", Provider: "codex", Quota: poolTestQuota("codex", "25", now)}
	b := &Auth{ID: "pool-other-model", Provider: "codex"}
	m.auths = map[string]*Auth{a.ID: a, b.ID: b}
	models := registry.GetGlobalRegistry()
	models.RegisterClient(a.ID, a.Provider, []*registry.ModelInfo{{ID: "pool-model"}})
	models.RegisterClient(b.ID, b.Provider, []*registry.ModelInfo{{ID: "other-pool-model"}})
	t.Cleanup(func() { models.UnregisterClient(a.ID); models.UnregisterClient(b.ID) })
	h := m.pooledQuotaHeaders(context.Background(), []string{"codex"}, "pool-model", coreexecutor.Options{}, nil, now)
	if got := h.Get("X-Codex-Primary-Used-Percent"); got != "25" {
		t.Fatalf("unrelated model affected pool: %v", h)
	}
}

func TestPooledClaudeUsageIncludesAccountsWithoutInference(t *testing.T) {
	now := time.Unix(1800000000, 0)
	reset := strconv.FormatInt(now.Add(time.Hour).Unix(), 10)
	snapshot := func(used string) *SubscriptionQuota {
		return &SubscriptionQuota{ObservedAt: now, UsageSignals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": used,
			"Anthropic-Ratelimit-Unified-7d-Reset":       reset,
		}}
	}
	m := NewManager(nil, nil, nil)
	m.auths = map[string]*Auth{
		"unused": {ID: "unused", Provider: "claude", Quota: QuotaState{Subscription: snapshot("0.75")}},
		"used":   {ID: "used", Provider: "claude", Quota: QuotaState{Subscription: snapshot("0.12")}},
	}
	read := func() http.Header {
		return m.pooledQuotaHeaders(context.Background(), []string{"claude"}, "", coreexecutor.Options{}, nil, now)
	}
	if got := read().Get("Anthropic-Ratelimit-Unified-7d-Utilization"); got != "0.435" {
		t.Fatalf("weekly pool = %q; want 0.435", got)
	}
	if got := read().Get("Anthropic-Ratelimit-Unified-5h-Utilization"); got != "" {
		t.Fatalf("unknown session quota invented: %q", got)
	}
	clone := m.auths["unused"].Quota.Clone()
	clone.Subscription.UsageSignals["Anthropic-Ratelimit-Unified-7d-Utilization"] = "0"
	if got := read().Get("Anthropic-Ratelimit-Unified-7d-Utilization"); got != "0.435" {
		t.Fatalf("clone changed live usage: %q", got)
	}
	m.auths["unused"].Quota.Subscription.ObservedAt = now.Add(-6 * time.Minute)
	if got := read().Get("Anthropic-Ratelimit-Unified-7d-Utilization"); got != "" {
		t.Fatalf("stale account snapshot counted: %q", got)
	}
}

func TestPooledClaudePrefersNewerAccountObservation(t *testing.T) {
	now := time.Unix(1800000000, 0)
	q := poolTestQuota("claude", "0.2", now.Add(-time.Minute))
	q.Subscription = &SubscriptionQuota{ObservedAt: now, UsageSignals: map[string]string{}}
	m := NewManager(nil, nil, nil)
	m.auths = map[string]*Auth{"a": {ID: "a", Provider: "claude", Quota: q}}
	if got := m.pooledQuotaHeaders(context.Background(), []string{"claude"}, "", coreexecutor.Options{}, nil, now); len(got) != 0 {
		t.Fatalf("newer unknown usage replaced with old inference: %v", got)
	}
	m.auths["a"].Quota = poolTestQuota("claude", "0.3", now)
	m.auths["a"].Quota.Subscription = &SubscriptionQuota{ObservedAt: now.Add(-time.Minute), UsageSignals: map[string]string{}}
	if got := m.pooledQuotaHeaders(context.Background(), []string{"claude"}, "", coreexecutor.Options{}, nil, now).Get("Anthropic-Ratelimit-Unified-5h-Utilization"); got != "0.3" {
		t.Fatalf("newer inference discarded: %q", got)
	}
}
