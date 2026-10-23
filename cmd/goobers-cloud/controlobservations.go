package main

import (
	"github.com/goobers/goobers/internal/cloud/epoch"
	"github.com/goobers/goobers/internal/cloud/providerobs"
	"github.com/goobers/goobers/internal/localscheduler"
)

// observationsFor persists state across controller epochs (control.Observations,
// fenced as goobers_controller). The Observations seam stays PENDING, and
// constructEpoch refuses naming "provider observations", until the L1 scheduler
// exists to produce into a ledger: persisting an unfed ledger would capture an
// empty snapshot with every test green. When L1 lands it must call
//
//	epochWiring.Observations = observationsFor(sameQuotaState, report)
//
// with the exact ProviderQuotaState its scheduler and provider clients record
// into.
func observationsFor(state *localscheduler.ProviderQuotaState, report func(error)) func() epoch.Observations {
	quota := &providerobs.Quota{State: state, Report: report}
	return func() epoch.Observations {
		return epoch.Observations{Restore: quota.Restore, Capture: quota.Capture}
	}
}
