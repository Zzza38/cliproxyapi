package cliproxy

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotaforecast"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

func (s *Service) configureSubscriptionHistory(ctx context.Context) {
	if strings.TrimSpace(s.configPath) == "" {
		return
	}
	path, errPath := filepath.Abs(s.configPath)
	if errPath != nil {
		log.WithError(errPath).Warn("resolve quota history path")
		return
	}
	store, errOpen := quotahistory.Open(filepath.Join(filepath.Dir(path), "quota-history"), time.Now())
	if errOpen != nil {
		log.WithError(errOpen).Error("open durable quota history")
		return
	}
	s.coreManager.SetSubscriptionHistoryStore(store)
	trainer := quotaforecast.NewTrainer(forecastSource{manager: s.coreManager, store: store}, filepath.Join(filepath.Dir(path), "quota-forecast.json"))
	s.coreManager.SetQuotaForecaster(trainer)
	go trainer.Run(ctx)
	usage.RegisterNamedPlugin("subscription-token-history", &subscriptionTokenRecorder{store: store})
}

type subscriptionTokenRecorder struct{ store *quotahistory.Store }

func (p *subscriptionTokenRecorder) HandleUsage(_ context.Context, record usage.Record) {
	detail := usage.EnsureTokenBreakdownForProvider(record.Detail, record.Provider, record.ExecutorType).TokenBreakdown
	at := record.RequestedAt
	if at.IsZero() {
		at = time.Now()
	}
	_, err := p.store.RecordTokens([]quotahistory.TokenPoint{{ID: record.RequestID, Provider: record.Provider, At: at.UnixMilli(), Input: detail.Input.TotalTokens, Output: detail.Output.TotalTokens, CacheRead: detail.Input.CacheReadTokens, CacheWrite: detail.Input.CacheWriteTokens, Total: detail.TotalTokens}}, time.Now())
	if err != nil {
		log.WithError(err).Error("persist token usage history")
	}
}

// forecastSource feeds the forecast trainer the same credentials and plan
// units the dashboard pools.
type forecastSource struct {
	manager *coreauth.Manager
	store   *quotahistory.Store
}

func historyID(auth *coreauth.Auth) string {
	name := auth.FileName
	if name == "" {
		name = auth.ID
	}
	return auth.Provider + ":" + name
}

func (f forecastSource) History(now time.Time) quotahistory.History {
	live := map[string]bool{}
	for _, auth := range f.manager.List() {
		live[historyID(auth)] = true
	}
	return f.store.History(live, now)
}

func (f forecastSource) Tokens(now time.Time) []quotahistory.TokenPoint {
	return f.store.TokenPoints(now)
}

func (f forecastSource) Units() (map[string]float64, bool) {
	units := map[string]float64{}
	ready := true
	for _, auth := range f.manager.List() {
		if _, pooled := quotaforecast.WeeklyWindow[auth.Provider]; !pooled {
			continue
		}
		sub := auth.Quota.Subscription
		if sub != nil && sub.CapacityMultiplier > 0 {
			units[historyID(auth)] = sub.CapacityMultiplier
		} else if !auth.Disabled {
			ready = false
		}
	}
	return units, ready
}
