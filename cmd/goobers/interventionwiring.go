package main

import (
	"context"
	"log"
	"path/filepath"
	"sync"
	"sync/atomic"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/intervention"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/workflow"
)

type interventionDefinitionSet struct {
	featureDrivers   map[localscheduler.WorkflowIdentity]string
	runners          map[string]*runner.Runner
	legacyRunner     *runner.Runner
	machines         map[localscheduler.WorkflowIdentity]*workflow.Machine
	gooberDigests    map[localscheduler.WorkflowIdentity]string
	repoRefs         map[localscheduler.WorkflowIdentity]apiv1.RepoRef
	backlogObservers map[localscheduler.WorkflowIdentity]backlogObservationReader
}

type interventionDefinitionRegistry struct {
	current atomic.Pointer[interventionDefinitionSet]
}

func newInterventionDefinitionRegistry(definitions interventionDefinitionSet) *interventionDefinitionRegistry {
	registry := &interventionDefinitionRegistry{}
	registry.Replace(definitions)
	return registry
}

func (r *interventionDefinitionRegistry) Replace(definitions interventionDefinitionSet) {
	if r == nil {
		return
	}
	r.current.Store(&definitions)
}

func (r *interventionDefinitionRegistry) Snapshot() interventionDefinitionSet {
	if r == nil {
		return interventionDefinitionSet{}
	}
	definitions := r.current.Load()
	if definitions == nil {
		return interventionDefinitionSet{}
	}
	return *definitions
}

func interventionDefinitions(definitions *schedulerDefinitions, legacyRunner *runner.Runner) interventionDefinitionSet {
	return interventionDefinitionSet{
		runners:          definitions.Runners,
		featureDrivers:   featureDriverConfiguration(definitions.Entries),
		legacyRunner:     legacyRunner,
		machines:         definitions.Machines,
		gooberDigests:    definitions.GooberDigests,
		repoRefs:         definitions.RepoRefs,
		backlogObservers: admittedBacklogObservers(definitions.Entries),
	}
}

func newRunInterventionService(layout instance.Layout, setup *schedulerSetup, wg *sync.WaitGroup, errorLog *log.Logger) *intervention.Service {
	cfg := interventionServiceConfig(layout, setup.Interventions, setup.RunnerRegistry, setup.InstanceLog, wg, errorLog)
	cfg.StageRestartExecution = setup.InteractiveRestartExecution
	return intervention.New(cfg)
}

// interventionServiceConfig binds the intervention service to the daemon's
// definition registry, run-owner registry, pinned generations and claim
// ledger.
func interventionServiceConfig(
	layout instance.Layout,
	definitions *interventionDefinitionRegistry,
	runners *daemonRunnerRegistry,
	instanceLog *journal.InstanceLog,
	wg *sync.WaitGroup,
	errorLog *log.Logger,
) intervention.Config {
	return intervention.Config{
		Definitions: func() intervention.Definitions {
			snapshot := definitions.Snapshot()
			return intervention.Definitions{
				Runners: snapshot.runners, LegacyRunner: snapshot.legacyRunner, Machines: snapshot.machines,
				GooberDigests: snapshot.gooberDigests, RepoRefs: snapshot.repoRefs,
			}
		},
		Runners: runners,
		PinnedExecution: func(ctx context.Context, identity journal.RunIdentity) (intervention.Execution, error) {
			pinned, err := runners.executionGeneration(ctx, identity)
			if err != nil {
				return intervention.Execution{}, err
			}
			return intervention.Execution{Runner: pinned.runner, Machine: pinned.machine, GooberDigest: pinned.gooberDigest, RepoRef: pinned.repoRef}, nil
		},
		LocateRun: func(gaggles []string, runID string, includeLegacy bool) (string, string, error) {
			found, err := locateOwnedRun(layout, gaggles, runID, includeLegacy)
			return found.dir, found.gaggle, err
		},
		Claims:              interventionClaims{layout: layout, instanceLog: instanceLog},
		EngineDrivenRefusal: engineDrivenRefusal,
		WaitGroup:           wg,
		ErrorLog:            errorLog,
	}
}

// interventionClaims is the daemon claim ledger behind intervention.ClaimStore.
type interventionClaims struct {
	layout      instance.Layout
	instanceLog *journal.InstanceLog
}

func (c interventionClaims) History(runID string, fallbackProvider apiv1.Provider) ([]localscheduler.ClaimEntry, error) {
	return claimHistoryForRun(c.layout, runID, fallbackProvider)
}

func (c interventionClaims) Reclaim(claims []localscheduler.ClaimEntry, gaggle, runID, workflowName string) (bool, string, error) {
	var acquired bool
	var holder string
	lockPath := filepath.Join(c.layout.SchedulerDir(), claimLockFileName)
	err := withClaimLockForRun(lockPath, claimLockOperationIntervention, gaggle, runID, func() error {
		ledger, err := localscheduler.OpenClaimLedger(
			filepath.Join(c.layout.SchedulerDir(), claimLedgerFileName),
			localscheduler.WithInstanceLog(c.instanceLog),
		)
		if err != nil {
			return err
		}
		acquired, holder, err = ledger.ReclaimAll(claims, runID, workflowName, DefaultClaimLease)
		return err
	})
	return acquired, holder, err
}

func (c interventionClaims) Release(runID string) error {
	return releaseClaimsForRun(c.layout, c.instanceLog, runID)
}
