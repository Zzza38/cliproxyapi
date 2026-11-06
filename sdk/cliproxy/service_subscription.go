package cliproxy

import (
	"context"
	"time"
)

func (s *Service) refreshSubscriptionQuotas(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
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
