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
