package scaler

import "time"

// CooldownGuard prevents oscillation by enforcing a minimum time
// between consecutive scale actions.
//
// Without cooldown, the controller can thrash:
//   t=0:  TPOT breach → add decode pod
//   t=10: new pod not ready yet → TPOT still breached → add another
//   t=20: both pods ready → TPOT collapses → remove pod → loop
//
// Cooldown breaks this loop. Default: 120s.
type CooldownGuard struct {
	cooldown     time.Duration
	lastScaleAt  time.Time
}

// NewCooldownGuard creates a CooldownGuard with the given cooldown duration.
func NewCooldownGuard(cooldownSeconds int32) *CooldownGuard {
	return &CooldownGuard{
		cooldown: time.Duration(cooldownSeconds) * time.Second,
	}
}

// InCooldown returns true if a scale action was taken recently enough
// that we should skip this reconcile cycle.
func (c *CooldownGuard) InCooldown(lastScaleTime *time.Time) bool {
	if lastScaleTime == nil {
		return false
	}
	return time.Since(*lastScaleTime) < c.cooldown
}

// Remaining returns how much cooldown time is left.
func (c *CooldownGuard) Remaining(lastScaleTime *time.Time) time.Duration {
	if lastScaleTime == nil {
		return 0
	}
	elapsed := time.Since(*lastScaleTime)
	if elapsed >= c.cooldown {
		return 0
	}
	return c.cooldown - elapsed
}
