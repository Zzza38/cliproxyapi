package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	log "github.com/sirupsen/logrus"
)

type subscriptionRefreshState struct {
	epoch, version uint64
	next           time.Time
	failures       int
	inFlight       bool
}

func (m *Manager) subscriptionClock() time.Time {
	if m.subscriptionNow != nil {
		return m.subscriptionNow()
	}
	return time.Now()
}

func subscriptionRefreshInterval(provider string) time.Duration {
	if provider == "claude" {
		return 5 * time.Minute
	}
	return time.Minute
}

func subscriptionRetryDelay(provider string, failures int, err error) time.Duration {
	delay := subscriptionRefreshInterval(provider)
	for i := 1; i < failures && delay < 30*time.Minute; i++ {
		delay *= 2
	}
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	var retry interface{ RetryAfter() time.Duration }
	if errors.As(err, &retry) && retry.RetryAfter() > delay {
		delay = retry.RetryAfter()
	}
	return delay
}

// UsesSubscriptionRouting reports whether the active selector needs quota probes.
func (m *Manager) UsesSubscriptionRouting() bool {
	selector := m.Selector()
	if affinity, ok := selector.(*SessionAffinitySelector); ok {
		selector = affinity.fallback
	}
	_, ok := selector.(*SoonestResetFirstSelector)
	return ok
}

// RefreshSubscriptionQuotas probes credentials outside manager locks. Results
// are fenced against credential replacement and never persist auth material.
func (m *Manager) RefreshSubscriptionQuotas(ctx context.Context) {
	if m == nil {
		return
	}
	type job struct {
		auth     *Auth
		provider SubscriptionQuotaProvider
	}
	var jobs []job
	m.mu.RLock()
	for _, auth := range m.auths {
		if auth == nil || auth.Disabled || auth.Status == StatusDisabled || auth.AuthKind() == AuthKindAPIKey {
			continue
		}
		executor, _ := m.executorLocked(executorKeyFromAuth(auth))
		if provider, ok := executor.(SubscriptionQuotaProvider); ok {
			jobs = append(jobs, job{auth.Clone(), provider})
		}
	}
	m.mu.RUnlock()
	semaphore := make(chan struct{}, 4)
	var workers sync.WaitGroup
	for _, next := range jobs {
		select {
		case <-ctx.Done():
			workers.Wait()
			return
		case semaphore <- struct{}{}:
		}
		m.mu.Lock()
		if m.subscriptionRefresh == nil {
			m.subscriptionRefresh = make(map[string]*subscriptionRefreshState)
		}
		state := m.subscriptionRefresh[next.auth.ID]
		if state == nil || state.epoch != next.auth.RegistrationEpoch || state.version != next.auth.CredentialVersion {
			state = &subscriptionRefreshState{epoch: next.auth.RegistrationEpoch, version: next.auth.CredentialVersion}
			m.subscriptionRefresh[next.auth.ID] = state
		}
		if state.inFlight || m.subscriptionClock().Before(state.next) {
			m.mu.Unlock()
			<-semaphore
			continue
		}
		state.inFlight = true
		m.mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-semaphore }()
			quota, err := next.provider.FetchSubscriptionQuota(ctx, next.auth)
			m.mu.Lock()
			state.inFlight = false
			if err != nil {
				state.failures++
				state.next = m.subscriptionClock().Add(subscriptionRetryDelay(next.auth.Provider, state.failures, err))
			} else {
				state.failures = 0
				state.next = m.subscriptionClock().Add(subscriptionRefreshInterval(next.auth.Provider))
			}
			m.mu.Unlock()
			if err != nil {
				if ctx.Err() == nil {
					log.WithField("provider", next.auth.Provider).Debug("subscription quota probe failed")
				}
				return
			}
			if quota == nil {
				return
			}
			if quota.ObservedAt.IsZero() {
				quota.ObservedAt = m.subscriptionClock()
			}
			m.mu.Lock()
			accepted := false
			history := m.historyStore
			current := m.auths[next.auth.ID]
			if current != nil && current.RegistrationEpoch == next.auth.RegistrationEpoch && current.CredentialVersion == next.auth.CredentialVersion {
				previous := current.Quota.Subscription
				if previous == nil || quota.ObservedAt.After(previous.ObservedAt) {
					quota.ValidUntil = quota.ObservedAt.Add(subscriptionRefreshInterval(next.auth.Provider) + time.Minute)
					current.Quota.Subscription = quota.Clone()
					accepted = true
				}
			}
			m.mu.Unlock()
			if accepted && history != nil && quota.WeeklyKnown {
				name := next.auth.FileName
				if name == "" {
					name = next.auth.ID
				}
				_, errHistory := history.Record([]quotahistory.Point{{Credential: name, Provider: next.auth.Provider, At: quota.ObservedAt.UnixMilli(), Used: (1 - quota.WeeklyRemaining) * 100, Reset: quota.WeeklyResetAt.UnixMilli()}}, time.Now())
				if errHistory != nil {
					log.WithError(errHistory).Warn("persist subscription quota history")
				}
			}
		}()
	}
	workers.Wait()
}
