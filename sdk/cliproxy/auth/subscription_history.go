package auth

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotaforecast"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"
)

func (m *Manager) SetSubscriptionHistoryStore(store *quotahistory.Store) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.historyStore = store
}

func (m *Manager) SubscriptionHistoryStore() *quotahistory.Store {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.historyStore
}

// SetQuotaForecaster installs the pooled quota forecast trainer.
func (m *Manager) SetQuotaForecaster(trainer *quotaforecast.Trainer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quotaForecaster = trainer
}

// QuotaForecaster returns the pooled quota forecast trainer, if configured.
func (m *Manager) QuotaForecaster() *quotaforecast.Trainer {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.quotaForecaster
}
