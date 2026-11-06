package auth

import (
	"context"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	log "github.com/sirupsen/logrus"
)

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
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-semaphore }()
			quota, err := next.provider.FetchSubscriptionQuota(ctx, next.auth)
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
				quota.ObservedAt = time.Now()
			}
			m.mu.Lock()
			accepted := false
			history := m.historyStore
			current := m.auths[next.auth.ID]
			if current != nil && current.RegistrationEpoch == next.auth.RegistrationEpoch && current.CredentialVersion == next.auth.CredentialVersion {
				previous := current.Quota.Subscription
				if previous == nil || quota.ObservedAt.After(previous.ObservedAt) {
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
