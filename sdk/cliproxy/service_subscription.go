package cliproxy

import (
	"context"
	"time"
)

func (s *Service) refreshSubscriptionQuotas(ctx context.Context) {
	// The heartbeat only schedules due accounts; it does not itself trigger a probe.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		if s.coreManager != nil {
			s.coreManager.RefreshSubscriptionQuotas(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
