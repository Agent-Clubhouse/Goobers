package main

import (
	"fmt"
	"path/filepath"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/controlplane/planes"
	"github.com/goobers/goobers/internal/engineoperator"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
)

const claimLockOperationAPIUnpushedWork = "api.journal.unpushed-work"

type daemonRunJournalService struct {
	*planes.RunJournalService
	operatorMessages engineoperator.Service
	layout           instance.Layout
	log              *journal.InstanceLog
	definitions      *interventionDefinitionRegistry
}

func newDaemonRunJournalService(layout instance.Layout, log *journal.InstanceLog) *daemonRunJournalService {
	service := &daemonRunJournalService{layout: layout, log: log}
	service.RunJournalService = planes.NewRunJournalService(layout, service.claimedItemIDs)
	service.AttachWorkflowResolver(func(gaggle string, can func(apiv1.WorkflowSpec) bool) ([]string, error) {
		if service.definitions == nil {
			return nil, fmt.Errorf("the daemon's workflow definitions are not attached; refusing an unfiltered journal scan")
		}
		var names []string
		for identity, machine := range service.definitions.Snapshot().machines {
			if identity.Gaggle == gaggle && machine != nil && can(machine.Def.Spec) {
				names = append(names, identity.Workflow)
			}
		}
		sort.Strings(names)
		return names, nil
	})
	return service
}

func (s *daemonRunJournalService) runJournalGaggleOK(gaggle, runID string) bool {
	return s.RunJournalGaggleOK(gaggle, runID)
}

func gaggleMismatch(what string) error { return planes.GaggleMismatch(what) }

func (s *daemonRunJournalService) claimedItemIDs(gaggle, runID string) ([]string, error) {
	var ids []string
	lockPath := filepath.Join(s.layout.SchedulerDir(), claimLockFileName)
	err := withClaimLockForRun(lockPath, claimLockOperationAPIUnpushedWork, gaggle, runID, func() error {
		opts := []localscheduler.LedgerOption{}
		if s.log != nil {
			opts = append(opts, localscheduler.WithInstanceLog(s.log))
		}
		ledger, err := localscheduler.OpenClaimLedger(filepath.Join(s.layout.SchedulerDir(), claimLedgerFileName), opts...)
		if err != nil {
			return fmt.Errorf("open claim ledger: %w", err)
		}
		for _, entry := range ledger.ForRunAll(runID) {
			if entry.ItemID != "" {
				ids = append(ids, entry.ItemID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(ids)
	return ids, nil
}

var _ httpapi.RunJournalService = (*daemonRunJournalService)(nil)
