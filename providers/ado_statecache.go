package providers

import "time"

// ADOStateMetadataTTL bounds reuse of rarely changing process metadata. Cache
// layers use the same UTC windows, so a provider cannot extend the lifetime of
// a response reused by a stage transport. A clock moving backward expires it.
const ADOStateMetadataTTL = 5 * time.Minute

type adoStateCategoryCache struct {
	states []adoWorkItemState
	stored time.Time
}

func (c adoStateCategoryCache) fresh(now time.Time) bool {
	return c.states != nil && !c.stored.IsZero() && !now.Before(c.stored) &&
		c.stored.Truncate(ADOStateMetadataTTL).Equal(now.Truncate(ADOStateMetadataTTL))
}
