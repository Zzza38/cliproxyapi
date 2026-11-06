package auth

import (
	"context"
	"math"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const subscriptionQuotaMaxAge = 5 * time.Minute

// SubscriptionQuota is a read-only snapshot of subscription allowance. Reset
// credits influence ordering but are never redeemed by the router.
type SubscriptionQuota struct {
	ObservedAt          time.Time
	WeeklyResetAt       time.Time
	WeeklyRemaining     float64
	WeeklyKnown         bool
	CapacityMultiplier  float64
	BlockedUntil        time.Time
	ManualResetExpiries []time.Time
}

func (q *SubscriptionQuota) Clone() *SubscriptionQuota {
	if q == nil {
		return nil
	}
	copyQuota := *q
	copyQuota.ManualResetExpiries = append([]time.Time(nil), q.ManualResetExpiries...)
	return &copyQuota
}

// SubscriptionQuotaProvider reads allowance without consuming reset credits.
type SubscriptionQuotaProvider interface {
	FetchSubscriptionQuota(context.Context, *Auth) (*SubscriptionQuota, error)
}

// SoonestResetFirstSelector spends allowance with the earliest expiry first.
// Equal expiries prefer the greatest remaining allowance in base-plan units.
type SoonestResetFirstSelector struct {
	fallback RoundRobinSelector
	nowFunc  func() time.Time
}

func (s *SoonestResetFirstSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	now := time.Now()
	if s.nowFunc != nil {
		now = s.nowFunc()
	}
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	var best *Auth
	var bestExpiry time.Time
	bestRemaining := -1.0
	for _, candidate := range available {
		expiry, remaining := subscriptionRank(candidate.Quota.Subscription, now)
		if expiry.IsZero() {
			continue
		}
		if best == nil || expiry.Before(bestExpiry) || (expiry.Equal(bestExpiry) && remaining > bestRemaining) {
			best, bestExpiry, bestRemaining = candidate, expiry, remaining
		}
	}
	if best != nil {
		return best, nil
	}
	return s.fallback.Pick(ctx, provider, model, opts, available)
}

func subscriptionRank(q *SubscriptionQuota, now time.Time) (time.Time, float64) {
	if q == nil || !q.ObservedAt.Add(subscriptionQuotaMaxAge).After(now) {
		return time.Time{}, -1
	}
	expiry := q.WeeklyResetAt
	if !q.WeeklyKnown || !expiry.After(now) {
		expiry = time.Time{}
	}
	for _, reset := range q.ManualResetExpiries {
		if reset.After(now) && (expiry.IsZero() || reset.Before(expiry)) {
			expiry = reset
		}
	}
	remaining := -1.0
	if q.WeeklyKnown && q.WeeklyResetAt.After(now) && !math.IsNaN(q.WeeklyRemaining) && !math.IsInf(q.WeeklyRemaining, 0) {
		multiplier := q.CapacityMultiplier
		if multiplier <= 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
			multiplier = 1
		}
		remaining = math.Max(0, math.Min(1, q.WeeklyRemaining)) * multiplier
	}
	return expiry, remaining
}
