package auth

import "github.com/router-for-me/CLIProxyAPI/v8/internal/quotahistory"

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
