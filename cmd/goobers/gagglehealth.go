package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gagglehealth"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/readservice"
)

const maxGaggleHealthSnapshotRecords = 100

type daemonGaggleHealth struct {
	mu          sync.RWMutex
	root        string
	config      *instance.Config
	definitions *instance.ConfigSet
	retentions  map[string]time.Duration
	reads       *readservice.Local
	quota       *localscheduler.ProviderQuotaState
	instanceLog *journal.InstanceLog
	store       *gagglehealth.Store
	controller  *gagglehealth.Controller
	cancel      context.CancelFunc
	wg          sync.WaitGroup
}

func startDaemonGaggleHealth(ctx context.Context, root string, setup *schedulerSetup, reads *readservice.Local) (*daemonGaggleHealth, error) {
	watchCtx, cancel := context.WithCancel(ctx)
	health := &daemonGaggleHealth{
		root: root, config: setup.Config, reads: reads, quota: setup.ProviderQuota,
		instanceLog: setup.InstanceLog, cancel: cancel,
	}
	health.replaceDefinitions(setup.Definitions)
	store, err := gagglehealth.OpenStore(root, health.retention)
	if err != nil {
		cancel()
		return nil, err
	}
	controller, err := gagglehealth.NewController(store, health, gagglehealth.ControllerOptions{MaxConcurrent: 2})
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	health.store = store
	health.controller = controller
	if err := controller.Start(watchCtx, health.registrations(setup.Definitions)); err != nil {
		_ = store.Close()
		cancel()
		return nil, err
	}
	setup.InstanceLog.SetAppendObserver(func(journal.Event) { controller.WakeAll() })
	if setup.ReadModel != nil {
		health.wg.Add(1)
		go health.watchChanges(watchCtx, setup.ReadModel.Feed())
	}
	return health, nil
}

func (h *daemonGaggleHealth) Replace(definitions *instance.ConfigSet) error {
	if h == nil {
		return nil
	}
	registrations := h.registrations(definitions)
	retentions := resolvedHealthRetentions(definitions)
	h.mu.Lock()
	if err := h.controller.Reload(registrations); err != nil {
		h.mu.Unlock()
		return fmt.Errorf("reload gaggle health controllers: %w", err)
	}
	h.definitions = definitions
	h.retentions = retentions
	h.mu.Unlock()
	return nil
}

func (h *daemonGaggleHealth) Close() error {
	if h == nil {
		return nil
	}
	h.instanceLog.SetAppendObserver(nil)
	h.cancel()
	h.wg.Wait()
	h.controller.Stop()
	return h.store.Close()
}

func (h *daemonGaggleHealth) Health(_ context.Context, gaggle string) (apiv1.GaggleHealthResponse, error) {
	snapshot, err := h.store.Snapshot(gaggle)
	if err != nil {
		return apiv1.GaggleHealthResponse{}, err
	}
	status, ok := h.controller.Status(gaggle)
	if !ok {
		return apiv1.GaggleHealthResponse{}, fmt.Errorf("gaggle %q health controller is not active", gaggle)
	}
	controller := &apiv1.GaggleHealthControllerStatus{
		FreshAt:            status.FreshAt,
		LastError:          status.LastError,
		NextEvaluation:     status.NextEvaluation,
		ActiveFindingCount: status.ActiveFindings,
		Evaluating:         status.Evaluating,
	}
	if !status.LastSuccessfulEvaluation.IsZero() {
		lastSuccess := status.LastSuccessfulEvaluation
		controller.LastSuccessfulEvaluation = &lastSuccess
	}
	return apiv1.GaggleHealthResponse{
		SchemaVersion: apiv1.GaggleHealthSchemaVersion,
		Health:        snapshot,
		Controller:    controller,
	}, nil
}

func (h *daemonGaggleHealth) Snapshot(ctx context.Context, gaggle string, dependencies []gagglehealth.EvidenceDependency) (gagglehealth.Snapshot, error) {
	h.mu.RLock()
	if h.definitions == nil {
		h.mu.RUnlock()
		return gagglehealth.Snapshot{}, errors.New("gaggle health definitions unavailable")
	}
	definitions := h.definitions
	config := h.config
	h.mu.RUnlock()

	snapshot := gagglehealth.Snapshot{Gaggle: gaggle}
	found := false
	for _, configured := range definitions.Gaggles {
		if configured.Name == gaggle {
			found = true
			break
		}
	}
	if !found {
		return gagglehealth.Snapshot{}, fmt.Errorf("gaggle %q is not loaded", gaggle)
	}
	for _, dependency := range dependencies {
		switch dependency {
		case gagglehealth.EvidenceWorkflows:
			for _, workflow := range definitions.Workflows {
				if workflow.Spec.Gaggle == gaggle && len(snapshot.Workflows) < maxGaggleHealthSnapshotRecords {
					snapshot.Workflows = append(snapshot.Workflows, workflow.Name)
				}
			}
			sort.Strings(snapshot.Workflows)
		case gagglehealth.EvidenceRuns, gagglehealth.EvidenceWorkers:
			if h.reads == nil {
				return gagglehealth.Snapshot{}, errors.New("run evidence source unavailable")
			}
			runs, err := h.reads.ListRuns(ctx, readservice.RunListOptions{
				Gaggle: gaggle, Limit: maxGaggleHealthSnapshotRecords, ShowNoWork: true,
			})
			if err != nil {
				return gagglehealth.Snapshot{}, fmt.Errorf("read %s evidence: %w", dependency, err)
			}
			for _, run := range runs.Runs {
				summary := gagglehealth.RuntimeSummary{ID: run.ID, State: string(run.Phase), UpdatedAt: run.LastActivityAt}
				if dependency == gagglehealth.EvidenceRuns {
					snapshot.Runs = append(snapshot.Runs, summary)
				} else {
					summary.State = fmt.Sprintf("%s:%s", run.Phase, run.CurrentStage)
					snapshot.Workers = append(snapshot.Workers, summary)
				}
			}
		case gagglehealth.EvidenceClaims:
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(instance.NewLayout(h.root).SchedulerDir(), claimLedgerFileName))
			if err != nil {
				return gagglehealth.Snapshot{}, fmt.Errorf("open claim evidence: %w", err)
			}
			for _, claim := range ledger.Snapshot() {
				if claim.Gaggle == gaggle && len(snapshot.Claims) < maxGaggleHealthSnapshotRecords {
					snapshot.Claims = append(snapshot.Claims, gagglehealth.RuntimeSummary{
						ID: claim.ExternalID, State: "active", UpdatedAt: claim.ClaimedAt,
					})
				}
			}
		case gagglehealth.EvidenceRunners:
			if config != nil {
				for _, runner := range config.ResolvedRunners() {
					if len(snapshot.Runners) == maxGaggleHealthSnapshotRecords {
						break
					}
					snapshot.Runners = append(snapshot.Runners, gagglehealth.RuntimeSummary{
						ID: runner.Name, State: runner.Host, UpdatedAt: time.Time{},
					})
				}
			}
		case gagglehealth.EvidenceReconciliation:
			if h.reads == nil {
				return gagglehealth.Snapshot{}, errors.New("reconciliation evidence source unavailable")
			}
			if reload := h.reads.DefinitionReload(); reload != nil {
				snapshot.Reconciliation = append(snapshot.Reconciliation, gagglehealth.RuntimeSummary{
					ID: reload.ObservedDigest, State: reload.State, UpdatedAt: reload.ObservedAt,
				})
			}
		case gagglehealth.EvidenceProviders:
			if h.quota != nil {
				resetAt, ok := h.quota.ResetAt()
				if !ok {
					break
				}
				snapshot.Providers = append(snapshot.Providers, gagglehealth.RuntimeSummary{
					ID: "provider-quota", State: "observed", UpdatedAt: resetAt,
				})
			}
		}
	}
	return snapshot, nil
}

func (h *daemonGaggleHealth) watchChanges(ctx context.Context, feed *readmodel.Feed) {
	defer h.wg.Done()
	cursor, err := feed.Head(ctx)
	if err != nil {
		return
	}
	for {
		position, err := feed.Since(ctx, cursor, maxGaggleHealthSnapshotRecords)
		if err != nil {
			return
		}
		cursor = position.Cursor
		for _, change := range position.Changes {
			if change.Gaggle == "" {
				h.controller.WakeAll()
			} else {
				h.controller.Wake(change.Gaggle)
			}
		}
	}
}

func (h *daemonGaggleHealth) retention(gaggle string) (time.Duration, error) {
	h.mu.RLock()
	retention := h.retentions[gaggle]
	h.mu.RUnlock()
	if retention == 0 {
		retention, _ = time.ParseDuration(gagglehealth.DefaultPolicy().Thresholds.EvidenceRetention)
	}
	return retention, nil
}

func (h *daemonGaggleHealth) replaceDefinitions(definitions *instance.ConfigSet) {
	retentions := resolvedHealthRetentions(definitions)
	h.mu.Lock()
	h.definitions = definitions
	h.retentions = retentions
	h.mu.Unlock()
}

func resolvedHealthRetentions(definitions *instance.ConfigSet) map[string]time.Duration {
	retentions := make(map[string]time.Duration)
	if definitions != nil {
		for _, configured := range definitions.Gaggles {
			policy, err := gagglehealth.ResolvePolicy(configured.Spec.Health)
			if err != nil {
				continue
			}
			retention, _ := time.ParseDuration(policy.Thresholds.EvidenceRetention)
			retentions[configured.Name] = retention
		}
	}
	return retentions
}

func (h *daemonGaggleHealth) registrations(definitions *instance.ConfigSet) []gagglehealth.GaggleRegistration {
	if definitions == nil {
		return nil
	}
	registrations := make([]gagglehealth.GaggleRegistration, 0, len(definitions.Gaggles))
	for _, configured := range definitions.Gaggles {
		policy, err := gagglehealth.ResolvePolicy(configured.Spec.Health)
		if err != nil {
			continue
		}
		registrations = append(registrations, gagglehealth.GaggleRegistration{Name: configured.Name, Policy: policy})
	}
	return registrations
}

var _ gagglehealth.SnapshotSource = (*daemonGaggleHealth)(nil)
