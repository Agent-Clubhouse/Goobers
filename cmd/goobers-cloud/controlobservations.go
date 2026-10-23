package main

import (
	"github.com/goobers/goobers/internal/cloud/epoch"
	"github.com/goobers/goobers/internal/cloud/providerobs"
	"github.com/goobers/goobers/internal/localscheduler"
)

// controlProviderQuota is the controller's provider quota ledger. The scheduler
// and provider clients record into it; each epoch restores it from the
// predecessor's snapshot and persists it (control.Observations, fenced as
// goobers_controller) so a takeover does not re-spend a spent window.
var controlProviderQuota = localscheduler.NewProviderQuotaState()

func init() {
	epochWiring.Observations = func() epoch.Observations {
		quota := providerobs.Quota{State: controlProviderQuota}
		return epoch.Observations{Restore: quota.Restore, Capture: quota.Capture}
	}
}
