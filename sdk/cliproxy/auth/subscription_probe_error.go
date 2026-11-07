package auth

import (
	"fmt"
	"time"
)

// SubscriptionProbeError carries read-only endpoint throttling without putting
// the account's inference requests into cooldown.
type SubscriptionProbeError struct {
	Status int
	Retry  time.Duration
}

func (e *SubscriptionProbeError) Error() string {
	return fmt.Sprintf("subscription quota: HTTP %d", e.Status)
}
func (e *SubscriptionProbeError) RetryAfter() time.Duration { return e.Retry }
