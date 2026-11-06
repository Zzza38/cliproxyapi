package auth

import (
	"context"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type subscriptionTestExecutor struct {
	schedulerTestExecutor
	fetch func(context.Context, *Auth) (*SubscriptionQuota, error)
}

func (e *subscriptionTestExecutor) FetchSubscriptionQuota(ctx context.Context, auth *Auth) (*SubscriptionQuota, error) {
	return e.fetch(ctx, auth)
}

func TestSubscriptionRefreshAffectsManagerSelection(t *testing.T) {
	now := time.Now()
	m := NewManager(nil, &SoonestResetFirstSelector{}, nil)
	dir := t.TempDir()
	store, errOpen := quotahistory.Open(dir, now)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	m.SetSubscriptionHistoryStore(store)
	m.RegisterExecutor(&subscriptionTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "codex"}, fetch: func(_ context.Context, a *Auth) (*SubscriptionQuota, error) {
		reset := 24 * time.Hour
		if a.ID == "b" {
			reset = time.Hour
		}
		return &SubscriptionQuota{ObservedAt: now, WeeklyKnown: true, WeeklyResetAt: now.Add(reset), WeeklyRemaining: .5, CapacityMultiplier: 10}, nil
	}})
	for _, id := range []string{"a", "b"} {
		if _, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex"}); err != nil {
			t.Fatal(err)
		}
	}
	m.RefreshSubscriptionQuotas(context.Background())
	reopened, errReopen := quotahistory.Open(dir, now)
	if errReopen != nil {
		t.Fatal(errReopen)
	}
	if got := reopened.History(map[string]bool{"codex:a": true, "codex:b": true}, now); len(got.Credentials) != 2 {
		t.Fatalf("background observations were not persisted: %+v", got)
	}
	if m.useSchedulerFastPath() {
		t.Fatal("subscription routing must use live quota snapshots")
	}
	got, _, err := m.pickNext(context.Background(), "codex", "", cliproxyexecutor.Options{}, nil)
	if err != nil || got.ID != "b" {
		t.Fatalf("manager selected %v, %v; want b", got, err)
	}
	cloned, _ := m.GetByID("b")
	cloned.Quota.Subscription.WeeklyRemaining = 0
	current, _ := m.GetByID("b")
	if current.Quota.Subscription.WeeklyRemaining != .5 {
		t.Fatal("snapshot clone mutates manager")
	}
}

func TestSubscriptionRefreshDoesNotOverwriteReplacedCredential(t *testing.T) {
	m := NewManager(nil, &SoonestResetFirstSelector{}, nil)
	started, resume := make(chan struct{}), make(chan struct{})
	m.RegisterExecutor(&subscriptionTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "codex"}, fetch: func(context.Context, *Auth) (*SubscriptionQuota, error) {
		close(started)
		<-resume
		return &SubscriptionQuota{ObservedAt: time.Now(), WeeklyKnown: true}, nil
	}})
	if _, err := m.Register(context.Background(), &Auth{ID: "a", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { m.RefreshSubscriptionQuotas(context.Background()); close(done) }()
	<-started
	if _, err := m.Register(context.Background(), &Auth{ID: "a", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	close(resume)
	<-done
	current, _ := m.GetByID("a")
	if current.Quota.Subscription != nil {
		t.Fatal("stale probe attached to replacement credential")
	}
}

func TestSubscriptionResetInvalidatesInFlightProbe(t *testing.T) {
	m := NewManager(nil, &SoonestResetFirstSelector{}, nil)
	started, resume := make(chan struct{}), make(chan struct{})
	m.RegisterExecutor(&subscriptionTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "codex"}, fetch: func(context.Context, *Auth) (*SubscriptionQuota, error) {
		observed := time.Unix(100, 0)
		close(started)
		<-resume
		return &SubscriptionQuota{ObservedAt: observed, WeeklyKnown: true, BlockedUntil: observed.Add(time.Hour)}, nil
	}})
	if _, err := m.Register(context.Background(), &Auth{ID: "a", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { m.RefreshSubscriptionQuotas(context.Background()); close(done) }()
	<-started
	if _, _, err := m.ResetQuota(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	close(resume)
	<-done
	current, _ := m.GetByID("a")
	if current.Quota.Subscription == nil || !current.Quota.Subscription.BlockedUntil.IsZero() {
		t.Fatal("pre-reset observation restored exhaustion")
	}
}

func TestClaudeSubscriptionRefreshCadenceAndBackoff(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	m := NewManager(nil, &SoonestResetFirstSelector{}, nil)
	m.subscriptionNow = func() time.Time { return now }
	calls := 0
	limited := false
	m.RegisterExecutor(&subscriptionTestExecutor{schedulerTestExecutor: schedulerTestExecutor{provider: "claude"}, fetch: func(context.Context, *Auth) (*SubscriptionQuota, error) {
		calls++
		if limited {
			return nil, &SubscriptionProbeError{Status: 429}
		}
		return &SubscriptionQuota{ObservedAt: now, WeeklyKnown: true, WeeklyResetAt: now.Add(24 * time.Hour), WeeklyRemaining: .5}, nil
	}})
	if _, err := m.Register(context.Background(), &Auth{ID: "a", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	start := now
	for minute := 0; minute < 60; minute++ {
		now = start.Add(time.Duration(minute) * time.Minute)
		m.RefreshSubscriptionQuotas(context.Background())
	}
	if calls != 12 {
		t.Fatalf("hourly probes = %d, want 12", calls)
	}
	current, _ := m.GetByID("a")
	if !subscriptionQuotaFresh(current.Quota.Subscription, now) {
		t.Fatal("snapshot expires before next scheduled probe")
	}
	limited = true
	now = start.Add(time.Hour)
	m.RefreshSubscriptionQuotas(context.Background())
	state := m.subscriptionRefresh["a"]
	if !state.next.Equal(now.Add(5 * time.Minute)) {
		t.Fatalf("first backoff=%v", state.next.Sub(now))
	}
	for _, wait := range []time.Duration{10 * time.Minute, 20 * time.Minute, 30 * time.Minute, 30 * time.Minute} {
		before := calls
		now = state.next.Add(-time.Second)
		m.RefreshSubscriptionQuotas(context.Background())
		if calls != before {
			t.Fatal("probe attempted during cooldown")
		}
		now = state.next
		m.RefreshSubscriptionQuotas(context.Background())
		if state.next.Sub(now) != wait {
			t.Fatalf("backoff=%v, want %v", state.next.Sub(now), wait)
		}
	}
	limited = false
	now = state.next
	m.RefreshSubscriptionQuotas(context.Background())
	if state.failures != 0 || state.next.Sub(now) != 5*time.Minute {
		t.Fatal("successful read did not restore normal cadence")
	}
}

func TestSubscriptionBackoffHonorsRetryAfter(t *testing.T) {
	if got := subscriptionRetryDelay("claude", 1, &SubscriptionProbeError{Status: 429, Retry: 45 * time.Minute}); got != 45*time.Minute {
		t.Fatal(got)
	}
	if got := subscriptionRetryDelay("claude", 1, &SubscriptionProbeError{Status: 429}); got != 5*time.Minute {
		t.Fatal(got)
	}
	if got := subscriptionRetryDelay("codex", 1, nil); got != time.Minute {
		t.Fatal(got)
	}
}
