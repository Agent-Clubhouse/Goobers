package runtimewiring

import (
	"context"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/providers"
)

// ProviderQuotaAccounting adapts provider quota operations to the shared counter authority.
type ProviderQuotaAccounting struct {
	Mu          sync.Mutex
	State       *localscheduler.ProviderQuotaState
	Prepaid     *localscheduler.ProviderPollReservation
	Outstanding []localscheduler.ProviderPollReservation
}

// AcquireQuotaRequest reserves quota for a provider request.
func (a *ProviderQuotaAccounting) AcquireQuotaRequest(_ context.Context, provider providers.ProviderKind) error {
	a.Mu.Lock()
	defer a.Mu.Unlock()
	if a.Prepaid != nil {
		a.Outstanding = append(a.Outstanding, *a.Prepaid)
		a.Prepaid = nil
		return nil
	}
	decision := a.State.ReserveCurrentPolls(apiv1.Provider(provider), 1)
	if decision.Allowed == 0 {
		return &localscheduler.ProviderPollBudgetError{
			Provider:  decision.Provider,
			Remaining: decision.RemainingBefore,
			Requested: 1,
			ResetAt:   decision.ResetAt,
		}
	}
	reservation, _ := decision.Reservation()
	a.Outstanding = append(a.Outstanding, reservation)
	return nil
}

// ObserveQuota records provider quota observations.
func (a *ProviderQuotaAccounting) ObserveQuota(_ context.Context, observation providers.QuotaObservation) {
	a.Mu.Lock()
	var reservation localscheduler.ProviderPollReservation
	if len(a.Outstanding) > 0 {
		reservation = a.Outstanding[0]
		a.Outstanding = a.Outstanding[1:]
	}
	a.Mu.Unlock()

	provider := apiv1.Provider(observation.Provider)
	if observation.Cached {
		a.State.RefundReservation(reservation)
		return
	}
	if observation.Known {
		a.State.Record(provider, observation.Remaining, observation.Reset)
	}
}

// RefundUnused returns unused quota to the shared authority.
func (a *ProviderQuotaAccounting) RefundUnused() {
	a.Mu.Lock()
	if a.Prepaid == nil {
		a.Mu.Unlock()
		return
	}
	reservation := *a.Prepaid
	a.Prepaid = nil
	a.Mu.Unlock()
	a.State.RefundReservation(reservation)
}
