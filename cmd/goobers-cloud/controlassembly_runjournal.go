package main

import (
	"context"
	"errors"
	"sort"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/controlplane/planes"
	"github.com/goobers/goobers/internal/instance"
)

// runJournalClaimLookupTimeout bounds one stranded-work claims read.
const runJournalClaimLookupTimeout = 10 * time.Second

// constructRunJournal composes the cross-run journal READ plane (run phase,
// conflict touches, unpushed work, escalation candidates, branch ownership)
// exactly as the daemon does (newDaemonRunJournalService), over the control
// role's authoritative stores: the journal through journalpg, a run's claims
// through the claims plane, escalation reads through the cloud Reader, and
// workflow definitions from the applied generation. Writes are not here; the
// worker's journal writes go through the Journal seam. Containment is the
// plane's and the handler's: the asking run is the pod principal's
// (podBodyRunContained), it and any target must belong to the named gaggle,
// and stranded work is answered for the claims the run holds, never for the
// items the request names. journal.* and control.claim are controller-only,
// so this is a controller seam.
func constructRunJournal(ctx context.Context, _ databaseServices, _ controlAuth, _ *lease.Manager, _ func() *lease.Handle, p *controlPlanes) error {
	if p.journalReader == nil {
		return errors.New("control run journal requires the journal plane's reader")
	}
	if p.claimedItems == nil {
		return errors.New("control run journal requires the claims plane")
	}
	if p.Reader == nil {
		return errors.New("control run journal requires the cloud reader")
	}
	if p.appliedSet == nil {
		return errors.New("control run journal requires the applied configuration")
	}
	claimedItems, appliedSet := p.claimedItems, p.appliedSet
	// The plane's callback carries no context; each lookup is bounded and
	// cancelled with the serving process. An error is returned, never read as
	// "no claims", so the stranded-work answer fails closed.
	claimedIDs := func(_, runID string) ([]string, error) {
		lookup, cancel := context.WithTimeout(ctx, runJournalClaimLookupTimeout)
		defer cancel()
		ids, err := claimedItems(lookup, runID)
		if err != nil {
			return nil, err
		}
		sort.Strings(ids)
		return ids, nil
	}
	service := planes.NewRunJournalService(instance.Layout{}, claimedIDs, p.journalReader)
	service.AttachReads(p.Reader)
	service.AttachWorkflowResolver(func(gaggle string, can func(apiv1.WorkflowSpec) bool) ([]string, error) {
		return appliedWorkflows(appliedSet, gaggle, can)
	})
	p.RunJournal = service
	return nil
}

// appliedWorkflows names the applied generation's workflows of gaggle that can
// contribute to a windowed route. Before any generation is applied it refuses,
// as the daemon does without definitions, rather than scanning unfiltered.
func appliedWorkflows(appliedSet func() (*instance.ConfigSet, bool), gaggle string, can func(apiv1.WorkflowSpec) bool) ([]string, error) {
	set, ok := appliedSet()
	if !ok {
		return nil, errors.New("no configuration generation is applied; refusing an unfiltered journal scan")
	}
	var names []string
	for _, workflow := range set.Workflows {
		if workflow.Spec.Gaggle == gaggle && can(workflow.Spec) {
			names = append(names, workflow.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}
