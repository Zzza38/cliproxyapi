package cliproxy

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

func (s *Service) configureSubscriptionHistory() {
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
