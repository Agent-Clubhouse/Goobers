package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
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

type gaggleHealthEvidenceReader interface {
	ListRuns(context.Context, readservice.RunListOptions) (readservice.RunList, error)
	GetRun(context.Context, string) (readservice.RunDetail, error)
	StageAttempts(context.Context, string, string) (readservice.AttemptList, error)
	DefinitionReload() *readservice.DefinitionReloadStatus
}

type daemonGaggleHealth struct {
	mu          sync.RWMutex
	root        string
	config      *instance.Config
	definitions *instance.ConfigSet
	retentions  map[string]time.Duration
	reads       gaggleHealthEvidenceReader
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
	store.SetAppendObserver(health.observeHealthTransition)
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
	h.store.SetAppendObserver(nil)
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

	if !hasGaggle(definitions, gaggle) {
		return gagglehealth.Snapshot{}, fmt.Errorf("gaggle %q is not loaded", gaggle)
	}

	builder := gaggleHealthSnapshotBuilder{
		health: h, ctx: ctx, gaggle: gaggle, definitions: definitions, config: config,
		snapshot: gagglehealth.Snapshot{Gaggle: gaggle},
	}
	if needsRunEvidence(dependencies) {
		runs, err := h.readRuns(ctx, gaggle)
		if err != nil {
			return gagglehealth.Snapshot{}, err
		}
		builder.runs = runs
	}
	for _, dependency := range dependencies {
		if err := builder.append(dependency); err != nil {
			return gagglehealth.Snapshot{}, err
		}
	}
	return builder.snapshot, nil
}

type gaggleHealthSnapshotBuilder struct {
	health      *daemonGaggleHealth
	ctx         context.Context
	gaggle      string
	definitions *instance.ConfigSet
	config      *instance.Config
	runs        []readservice.RunSummary
	snapshot    gagglehealth.Snapshot
}

func hasGaggle(definitions *instance.ConfigSet, gaggle string) bool {
	for _, configured := range definitions.Gaggles {
		if configured.Name == gaggle {
			return true
		}
	}
	return false
}

func needsRunEvidence(dependencies []gagglehealth.EvidenceDependency) bool {
	for _, dependency := range dependencies {
		switch dependency {
		case gagglehealth.EvidenceRuns, gagglehealth.EvidenceClaims,
			gagglehealth.EvidenceRunners, gagglehealth.EvidenceWorkers:
			return true
		}
	}
	return false
}

func (h *daemonGaggleHealth) readRuns(ctx context.Context, gaggle string) ([]readservice.RunSummary, error) {
	if h.reads == nil {
		return nil, errors.New("run evidence source unavailable")
	}
	result, err := h.reads.ListRuns(ctx, readservice.RunListOptions{
		Gaggle: gaggle, Limit: maxGaggleHealthSnapshotRecords, ShowNoWork: true,
	})
	if err != nil {
		return nil, fmt.Errorf("read run evidence: %w", err)
	}
	return result.Runs, nil
}

func (b *gaggleHealthSnapshotBuilder) append(dependency gagglehealth.EvidenceDependency) error {
	switch dependency {
	case gagglehealth.EvidenceWorkflows:
		return b.appendWorkflows()
	case gagglehealth.EvidenceRuns:
		b.appendRuns()
	case gagglehealth.EvidenceClaims:
		return b.appendClaims()
	case gagglehealth.EvidenceRunners:
		return b.appendRunners()
	case gagglehealth.EvidenceReconciliation:
		return b.appendReconciliation()
	case gagglehealth.EvidenceProviders:
		b.appendProviders()
	case gagglehealth.EvidenceWorkers:
		return b.health.appendWorkerEvidence(b.ctx, b.runs, &b.snapshot)
	}
	return nil
}

func (b *gaggleHealthSnapshotBuilder) appendWorkflows() error {
	evaluations, err := localscheduler.ReadTriggerEvaluations(instance.NewLayout(b.health.root).SchedulerDir())
	if err != nil {
		return fmt.Errorf("read trigger evidence: %w", err)
	}
	for _, workflow := range b.definitions.Workflows {
		if workflow.Spec.Gaggle != b.gaggle || len(b.snapshot.Workflows) >= maxGaggleHealthSnapshotRecords {
			continue
		}
		state := "enabled"
		if workflow.Spec.Enabled != nil && !*workflow.Spec.Enabled {
			state = "disabled"
		}
		types := make([]string, 0, len(workflow.Spec.Triggers))
		for _, trigger := range workflow.Spec.Triggers {
			types = append(types, string(trigger.Type))
		}
		lastEval := evaluations[localscheduler.WorkflowIdentity{Gaggle: b.gaggle, Workflow: workflow.Name}]
		b.snapshot.Workflows = append(b.snapshot.Workflows, gagglehealth.RuntimeSummary{
			ID: workflow.Name, State: state, UpdatedAt: lastEval,
			Attributes: map[string]string{"triggers": strings.Join(types, ",")},
		})
	}
	sort.Slice(b.snapshot.Workflows, func(i, j int) bool {
		return b.snapshot.Workflows[i].ID < b.snapshot.Workflows[j].ID
	})
	return nil
}

func (b *gaggleHealthSnapshotBuilder) appendRuns() {
	for _, run := range b.runs {
		b.snapshot.Runs = append(b.snapshot.Runs, gagglehealth.RuntimeSummary{
			ID: run.ID, State: string(run.Phase), UpdatedAt: run.LastActivityAt,
			Attributes: map[string]string{"workflow": run.Workflow, "currentStage": run.CurrentStage},
		})
	}
}

func (b *gaggleHealthSnapshotBuilder) appendClaims() error {
	ledger, err := localscheduler.OpenClaimLedger(filepath.Join(instance.NewLayout(b.health.root).SchedulerDir(), claimLedgerFileName))
	if err != nil {
		return fmt.Errorf("open claim evidence: %w", err)
	}
	runStates := make(map[string]string, len(b.runs))
	for _, run := range b.runs {
		runStates[run.ID] = string(run.Phase)
	}
	for _, claim := range ledger.Snapshot() {
		if claim.Gaggle != b.gaggle || len(b.snapshot.Claims) >= maxGaggleHealthSnapshotRecords {
			continue
		}
		intervention := "none"
		if runStates[claim.RunID] == string(journal.PhaseEscalated) {
			intervention = "required"
		}
		state := "active"
		if claim.SharedRevoked {
			state = "revoked"
		}
		b.snapshot.Claims = append(b.snapshot.Claims, gagglehealth.RuntimeSummary{
			ID: claim.ExternalID, State: state, UpdatedAt: claim.ClaimedAt,
			Attributes: map[string]string{
				"provider": claim.Provider, "runId": claim.RunID, "workflow": claim.Workflow,
				"expiresAt": claim.ExpiresAt.UTC().Format(time.RFC3339Nano), "intervention": intervention,
			},
		})
	}
	return nil
}

func (b *gaggleHealthSnapshotBuilder) appendRunners() error {
	if b.config != nil {
		for _, runner := range b.config.ResolvedRunners() {
			if len(b.snapshot.Runners) == maxGaggleHealthSnapshotRecords {
				break
			}
			b.snapshot.Runners = append(b.snapshot.Runners, gagglehealth.RuntimeSummary{
				ID: runner.Name, State: "available",
				Attributes: map[string]string{
					"host": runner.Host, "os": runnerOS(runner),
					"capabilities": strings.Join(runner.Provides.Capabilities, ","),
					"restrictions": strings.Join(runnerRestrictions(runner), ","),
				},
			})
		}
	}
	return b.health.appendLivePlacements(b.ctx, b.runs, &b.snapshot)
}

func (b *gaggleHealthSnapshotBuilder) appendReconciliation() error {
	if b.health.reads == nil {
		return errors.New("reconciliation evidence source unavailable")
	}
	if reload := b.health.reads.DefinitionReload(); reload != nil {
		b.snapshot.Reconciliation = append(b.snapshot.Reconciliation, gagglehealth.RuntimeSummary{
			ID: reload.ObservedDigest, State: reload.State, UpdatedAt: reload.ObservedAt,
		})
	}
	return nil
}

func (b *gaggleHealthSnapshotBuilder) appendProviders() {
	if b.health.quota == nil {
		return
	}
	resetAt, ok := b.health.quota.ResetAt()
	if !ok {
		return
	}
	b.snapshot.Providers = append(b.snapshot.Providers, gagglehealth.RuntimeSummary{
		ID: "provider-quota", State: "observed", UpdatedAt: resetAt,
	})
}

func (h *daemonGaggleHealth) appendLivePlacements(ctx context.Context, runs []readservice.RunSummary, snapshot *gagglehealth.Snapshot) error {
	for _, run := range runs {
		for _, active := range run.ActiveStages {
			if len(snapshot.Runners) >= maxGaggleHealthSnapshotRecords {
				return nil
			}
			if active.Kind != "stage" {
				continue
			}
			attempts, err := h.reads.StageAttempts(ctx, run.ID, active.Name)
			if err != nil {
				return fmt.Errorf("read runner placement for %s/%s: %w", run.ID, active.Name, err)
			}
			for i := len(attempts.Attempts) - 1; i >= 0; i-- {
				attempt := attempts.Attempts[i]
				if attempt.Number != active.Attempt || attempt.Placement == nil {
					continue
				}
				placement := attempt.Placement
				snapshot.Runners = append(snapshot.Runners, gagglehealth.RuntimeSummary{
					ID:    fmt.Sprintf("%s/%s/%d", run.ID, active.Name, active.Attempt),
					State: "placed", UpdatedAt: timeValue(attempt.StartedAt),
					Attributes: map[string]string{
						"runner": placement.Runner, "node": placement.Node, "host": placement.Host,
						"os": placement.OS, "worker": placement.Worker, "image": placement.Image, "pod": placement.Pod,
					},
				})
				break
			}
		}
	}
	return nil
}

func (h *daemonGaggleHealth) appendWorkerEvidence(ctx context.Context, runs []readservice.RunSummary, snapshot *gagglehealth.Snapshot) error {
	for _, run := range runs {
		if len(snapshot.Workers) >= maxGaggleHealthSnapshotRecords {
			return nil
		}
		detail, err := h.reads.GetRun(ctx, run.ID)
		if err != nil {
			return fmt.Errorf("read worker evidence for %s: %w", run.ID, err)
		}
		if detail.Lineage != nil && detail.Lineage.WorkspaceBranch != "" {
			snapshot.Workers = append(snapshot.Workers, gagglehealth.RuntimeSummary{
				ID: run.ID + "/worktree", State: "present", UpdatedAt: run.LastActivityAt,
				Attributes: map[string]string{
					"branch": detail.Lineage.WorkspaceBranch, "sha": detail.Lineage.WorkspaceBranchSHA,
				},
			})
		}
		appendAgentEvidence(snapshot, detail.AgentProgress)
	}
	return nil
}

func appendAgentEvidence(snapshot *gagglehealth.Snapshot, agents []readservice.AgentProgressSummary) {
	for _, agent := range agents {
		if len(snapshot.Workers) >= maxGaggleHealthSnapshotRecords {
			return
		}
		if agent.Worker {
			state := ""
			updatedAt := time.Time{}
			if agent.Current != nil {
				state = string(agent.Current.Lifecycle)
				if state == "" {
					state = string(agent.Current.Kind)
				}
				updatedAt = agent.Current.UpdatedAt
			}
			snapshot.Workers = append(snapshot.Workers, gagglehealth.RuntimeSummary{
				ID:    fmt.Sprintf("%s/%s/%s/%d", agent.RunID, agent.Stage, agent.AgentID, agent.Attempt),
				State: state, UpdatedAt: updatedAt,
				Attributes: map[string]string{"role": agent.Role, "fidelity": agent.Fidelity, "degraded": strconv.FormatBool(agent.Degraded)},
			})
		}
		appendAgentEvidence(snapshot, agent.Children)
	}
}

func runnerRestrictions(runner instance.RunnerEntry) []string {
	restrictions := make([]string, len(runner.Restrictions))
	for i, restriction := range runner.Restrictions {
		restrictions[i] = string(restriction)
	}
	return restrictions
}

func runnerOS(runner instance.RunnerEntry) string {
	if runner.Provides.OS == "" && runner.Host == instance.RunnerHostSelfName {
		return runtime.GOOS
	}
	return string(runner.Provides.OS)
}

func timeValue(value *time.Time) time.Time {
	if value == nil {
		return time.Time{}
	}
	return *value
}

func (h *daemonGaggleHealth) observeHealthTransition(event apiv1.GaggleHealthEvent) {
	if event.Type != apiv1.GaggleHealthRepairStartedEvent && event.Type != apiv1.GaggleHealthRepairFinishedEvent {
		return
	}
	if h.controller == nil {
		return
	}
	h.controller.Wake(event.Gaggle)
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
