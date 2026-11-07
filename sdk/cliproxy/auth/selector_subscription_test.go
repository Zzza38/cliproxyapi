package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func subscriptionAuth(id string, now time.Time, resetAfter time.Duration, remaining, multiplier float64) *Auth {
	return &Auth{ID: id, Provider: "codex", Quota: QuotaState{Subscription: &SubscriptionQuota{
		ObservedAt: now, WeeklyKnown: true, WeeklyResetAt: now.Add(resetAfter), WeeklyRemaining: remaining, CapacityMultiplier: multiplier,
	}}}
}

func TestSoonestResetFirstSelection(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name   string
		change func(a, b *Auth)
		want   string
	}{
		{"weekly deadline beats capacity", func(a, b *Auth) {}, "b"},
		{"manual reset deadline beats weekly", func(a, b *Auth) { a.Quota.Subscription.ManualResetExpiries = []time.Time{now.Add(time.Hour)} }, "a"},
		{"expired manual reset ignored", func(a, b *Auth) { a.Quota.Subscription.ManualResetExpiries = []time.Time{now.Add(-time.Hour)} }, "b"},
		{"tie uses absolute remaining allowance", func(a, b *Auth) {
			a.Quota.Subscription.WeeklyResetAt = b.Quota.Subscription.WeeklyResetAt
			a.Quota.Subscription.WeeklyRemaining = .25
			a.Quota.Subscription.CapacityMultiplier = 20
			b.Quota.Subscription.WeeklyRemaining = .4
			b.Quota.Subscription.CapacityMultiplier = 10
		}, "a"},
		{"disabled account excluded", func(a, b *Auth) { b.Disabled = true }, "a"},
		{"exhausted account excluded", func(a, b *Auth) { b.Quota.Subscription.BlockedUntil = now.Add(time.Hour) }, "a"},
		{"stale quota ignored", func(a, b *Auth) { b.Quota.Subscription.ObservedAt = now.Add(-10 * time.Minute) }, "a"},
		{"missing quota loses to known deadline", func(a, b *Auth) { b.Quota.Subscription = nil }, "a"},
		{"passed weekly deadline ignored", func(a, b *Auth) { b.Quota.Subscription.WeeklyResetAt = now.Add(-time.Minute) }, "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := subscriptionAuth("a", now, 6*24*time.Hour, .99, 50)
			b := subscriptionAuth("b", now, 5*24*time.Hour, .90, 10)
			tt.change(a, b)
			selector := &SoonestResetFirstSelector{nowFunc: func() time.Time { return now }}
			got, err := selector.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{b, a})
			if err != nil || got.ID != tt.want {
				t.Fatalf("pick = %v, %v; want %s", got, err, tt.want)
			}
		})
	}
}

func TestSoonestResetFirstUnknownFallsBackToRoundRobin(t *testing.T) {
	s := &SoonestResetFirstSelector{}
	for i, want := range []string{"a", "b", "a"} {
		got, err := s.Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{{ID: "b"}, {ID: "a"}})
		if err != nil || got.ID != want {
			t.Fatalf("pick %d = %v, %v; want %s", i, got, err, want)
		}
	}
}

func TestSoonestResetFirstKeepsSessionAndFailsOver(t *testing.T) {
	now := time.Now()
	a := subscriptionAuth("a", now, time.Hour, .5, 20)
	b := subscriptionAuth("b", now, 2*time.Hour, .9, 10)
	s := NewSessionAffinitySelector(&SoonestResetFirstSelector{})
	defer s.Stop()
	opts := cliproxyexecutor.Options{Headers: http.Header{"Session_id": []string{"subscription-session"}}}
	pick := func(want string) {
		t.Helper()
		got, err := s.Pick(context.Background(), "codex", "", opts, []*Auth{a, b})
		if err != nil || got.ID != want {
			t.Fatalf("pick = %v, %v; want %s", got, err, want)
		}
	}
	pick("a")
	b.Quota.Subscription.ManualResetExpiries = []time.Time{now.Add(time.Minute)}
	pick("a")
	a.Quota.Subscription.BlockedUntil = now.Add(time.Hour)
	pick("b")
}
