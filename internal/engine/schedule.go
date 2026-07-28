package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	sdktemporal "go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

const (
	scheduleIDPrefix         = "goobers-"
	maxScheduleCatchupWindow = 24 * time.Hour
	scheduleStateVersion     = 1
	scheduleStateWorkflow    = "goobers.schedule-state.v1"
	scheduleRPCTimeout       = 10 * time.Second
	scheduleReconcileLease   = time.Minute
	scheduleReconcilePoll    = 100 * time.Millisecond
)

// ScheduleSnapshot is one atomically-applied config snapshot. Runs are pinned
// engine inputs with RunID unset; the reconciler materializes each schedule
// trigger and Temporal supplies the per-fire workflow ID.
type ScheduleSnapshot struct {
	InstanceID       string     `json:"instanceId"`
	ConfigSHA        string     `json:"configSha"`
	ConfigGeneration uint64     `json:"configGeneration"`
	Runs             []RunInput `json:"runs"`
}

type scheduleReconcileState struct {
	Version            int                       `json:"version"`
	AppliedGeneration  uint64                    `json:"appliedGeneration,omitempty"`
	AppliedConfigSHA   string                    `json:"appliedConfigSha,omitempty"`
	ManagedScheduleIDs []string                  `json:"managedScheduleIds,omitempty"`
	Pending            *pendingScheduleReconcile `json:"pending,omitempty"`
}

type pendingScheduleReconcile struct {
	Snapshot   ScheduleSnapshot `json:"snapshot"`
	Owner      string           `json:"owner,omitempty"`
	LeaseUntil time.Time        `json:"leaseUntil,omitempty"`
}

// ScheduleReconciler materializes a config snapshot as Temporal Schedules.
type ScheduleReconciler struct {
	client        scheduleStore
	taskQueue     string
	catchupWindow time.Duration
}

// NewScheduleReconciler constructs a reconciler with explicit schedule policy.
func NewScheduleReconciler(
	service workflowservice.WorkflowServiceClient,
	namespace string,
	taskQueue string,
	catchupWindow time.Duration,
) (*ScheduleReconciler, error) {
	if service == nil {
		return nil, errors.New("engine: Temporal workflow service client is required")
	}
	if strings.TrimSpace(namespace) == "" {
		return nil, errors.New("engine: Temporal schedule namespace is required")
	}
	return newScheduleReconciler(newTemporalScheduleStore(service, namespace), taskQueue, catchupWindow)
}

func newScheduleReconciler(c scheduleStore, taskQueue string, catchupWindow time.Duration) (*ScheduleReconciler, error) {
	if strings.TrimSpace(taskQueue) == "" {
		return nil, errors.New("engine: Temporal schedule task queue is required")
	}
	if catchupWindow < 10*time.Second {
		return nil, errors.New("engine: Temporal schedule catch-up window must be at least 10 seconds")
	}
	if catchupWindow > maxScheduleCatchupWindow {
		return nil, fmt.Errorf("engine: Temporal schedule catch-up window must not exceed %s", maxScheduleCatchupWindow)
	}
	return &ScheduleReconciler{client: c, taskQueue: taskQueue, catchupWindow: catchupWindow}, nil
}

// ScheduleID deterministically identifies one trigger position in a workflow.
// The trigger ordinal counts schedule triggers only, so adding another trigger
// type does not replace an existing Temporal Schedule.
func ScheduleID(instanceID, gaggle, workflowName string, triggerOrdinal int) string {
	instanceHash := scheduleHash(instanceID)
	claimHash := scheduleHash(instanceID, gaggle, workflowName, fmt.Sprintf("%d", triggerOrdinal))
	return scheduleIDPrefix + instanceHash[:32] + "-" + claimHash[:32]
}

// ScheduleClaimID is the workflow ID Temporal derives for one schedule fire.
func ScheduleClaimID(scheduleID string, fireTime time.Time) string {
	return scheduleID + "-" + fireTime.UTC().Truncate(time.Second).Format(time.RFC3339)
}

func scheduleHash(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "%d:%s;", len(part), part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type desiredSchedule struct {
	options client.ScheduleOptions
}

// Reconcile advances one instance to a complete config generation. A paused
// Temporal state schedule serializes generations with conflict-token CAS and
// carries the authoritative IDs from the last applied snapshot.
func (r *ScheduleReconciler) Reconcile(ctx context.Context, snapshot ScheduleSnapshot) error {
	if _, err := r.desired(snapshot); err != nil {
		return err
	}
	owner, err := scheduleRequestID()
	if err != nil {
		return err
	}
	prefix := scheduleOwnedPrefix(snapshot.InstanceID)
	stateID := prefix + "state"

	for {
		revision, err := r.loadOrCreateState(ctx, stateID)
		if err != nil {
			return err
		}
		if err := validateScheduleState(revision.state, snapshot.InstanceID, prefix); err != nil {
			return err
		}
		if revision.state.Pending != nil {
			if revision.state.Pending.Owner != owner {
				if revision.state.Pending.Owner != "" &&
					time.Now().Before(revision.state.Pending.LeaseUntil) {
					timer := time.NewTimer(scheduleReconcilePoll)
					select {
					case <-ctx.Done():
						timer.Stop()
						return ctx.Err()
					case <-timer.C:
					}
					continue
				}
				next := revision.state
				pending := *revision.state.Pending
				pending.Owner = owner
				pending.LeaseUntil = time.Now().Add(scheduleReconcileLease)
				next.Pending = &pending
				if err := r.client.CompareAndSwapState(ctx, stateID, revision, next); err != nil {
					if errors.Is(err, errScheduleStateConflict) {
						continue
					}
					return err
				}
				continue
			}
			err := r.applyPending(ctx, stateID, revision)
			if errors.Is(err, errScheduleStateConflict) {
				continue
			}
			if err != nil {
				if releaseErr := r.releasePending(ctx, stateID, owner); releaseErr != nil {
					return errors.Join(err, releaseErr)
				}
				return err
			}
			continue
		}

		switch {
		case snapshot.ConfigGeneration < revision.state.AppliedGeneration:
			return nil
		case snapshot.ConfigGeneration == revision.state.AppliedGeneration:
			if snapshot.ConfigSHA != revision.state.AppliedConfigSHA {
				return fmt.Errorf(
					"engine: config generation %d is already applied with SHA %q, not %q",
					snapshot.ConfigGeneration,
					revision.state.AppliedConfigSHA,
					snapshot.ConfigSHA,
				)
			}
			return nil
		}

		next := revision.state
		pending := pendingScheduleReconcile{
			Snapshot:   snapshot,
			Owner:      owner,
			LeaseUntil: time.Now().Add(scheduleReconcileLease),
		}
		next.Pending = &pending
		if err := r.client.CompareAndSwapState(ctx, stateID, revision, next); err != nil {
			if errors.Is(err, errScheduleStateConflict) {
				continue
			}
			return err
		}
	}
}

func (r *ScheduleReconciler) releasePending(ctx context.Context, stateID, owner string) error {
	for {
		revision, err := r.client.LoadState(ctx, stateID)
		if err != nil {
			return fmt.Errorf("engine: reload Temporal schedule state %q after apply failure: %w", stateID, err)
		}
		if revision.state.Pending == nil || revision.state.Pending.Owner != owner {
			return nil
		}
		next := revision.state
		pending := *revision.state.Pending
		pending.Owner = ""
		pending.LeaseUntil = time.Time{}
		next.Pending = &pending
		if err := r.client.CompareAndSwapState(ctx, stateID, revision, next); err != nil {
			if errors.Is(err, errScheduleStateConflict) {
				continue
			}
			return err
		}
		return nil
	}
}

func (r *ScheduleReconciler) loadOrCreateState(ctx context.Context, stateID string) (scheduleStateRevision, error) {
	revision, err := r.client.LoadState(ctx, stateID)
	if err == nil {
		return revision, nil
	}
	if !isScheduleNotFound(err) {
		return scheduleStateRevision{}, fmt.Errorf("engine: load Temporal schedule state %q: %w", stateID, err)
	}

	state := scheduleReconcileState{Version: scheduleStateVersion}
	options := client.ScheduleOptions{
		ID: stateID,
		Spec: client.ScheduleSpec{Intervals: []client.ScheduleIntervalSpec{{
			Every: 24 * time.Hour,
		}}},
		Action: &client.ScheduleWorkflowAction{
			ID:        stateID,
			Workflow:  scheduleStateWorkflow,
			TaskQueue: r.taskQueue,
			RetryPolicy: &sdktemporal.RetryPolicy{
				MaximumAttempts: 1,
			},
		},
		Overlap:       enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
		CatchupWindow: r.catchupWindow,
		Paused:        true,
		Note:          "Goobers schedule reconciliation state",
	}
	err = r.client.CreateState(ctx, options, state)
	if err != nil &&
		!errors.Is(err, sdktemporal.ErrScheduleAlreadyRunning) {
		return scheduleStateRevision{}, fmt.Errorf("engine: create Temporal schedule state %q: %w", stateID, err)
	}
	revision, err = r.client.LoadState(ctx, stateID)
	if err != nil {
		return scheduleStateRevision{}, fmt.Errorf("engine: load created Temporal schedule state %q: %w", stateID, err)
	}
	return revision, nil
}

func (r *ScheduleReconciler) applyPending(
	ctx context.Context,
	stateID string,
	revision scheduleStateRevision,
) error {
	pending := revision.state.Pending
	if pending == nil {
		return errors.New("engine: cannot apply empty Temporal schedule snapshot")
	}
	desired, err := r.desired(pending.Snapshot)
	if err != nil {
		return err
	}

	desiredIDs := make([]string, 0, len(desired))
	for id := range desired {
		desiredIDs = append(desiredIDs, id)
	}
	previous := make(map[string]bool, len(revision.state.ManagedScheduleIDs))
	for _, id := range revision.state.ManagedScheduleIDs {
		previous[id] = true
	}
	sort.Strings(desiredIDs)
	for _, id := range desiredIDs {
		var err error
		revision, err = r.renewPendingLease(ctx, stateID, revision)
		if err != nil {
			return err
		}
		if err := r.client.ApplySchedule(ctx, desired[id].options, previous[id]); err != nil {
			return err
		}
	}
	previousIDs := append([]string(nil), revision.state.ManagedScheduleIDs...)
	sort.Strings(previousIDs)
	for _, id := range previousIDs {
		if _, ok := desired[id]; ok {
			continue
		}
		var err error
		revision, err = r.renewPendingLease(ctx, stateID, revision)
		if err != nil {
			return err
		}
		if err := r.client.DeleteSchedule(ctx, id, pending.Snapshot.ConfigGeneration); err != nil && !isScheduleNotFound(err) {
			return fmt.Errorf("engine: delete Temporal schedule %q: %w", id, err)
		}
	}

	next := revision.state
	next.AppliedGeneration = pending.Snapshot.ConfigGeneration
	next.AppliedConfigSHA = pending.Snapshot.ConfigSHA
	next.ManagedScheduleIDs = desiredIDs
	next.Pending = nil
	return r.client.CompareAndSwapState(ctx, stateID, revision, next)
}

func (r *ScheduleReconciler) renewPendingLease(
	ctx context.Context,
	stateID string,
	revision scheduleStateRevision,
) (scheduleStateRevision, error) {
	pending := revision.state.Pending
	if pending == nil || pending.Owner == "" {
		return scheduleStateRevision{}, errScheduleStateConflict
	}
	next := revision.state
	renewed := *pending
	renewed.LeaseUntil = time.Now().Add(scheduleReconcileLease)
	next.Pending = &renewed
	if err := r.client.CompareAndSwapState(ctx, stateID, revision, next); err != nil {
		return scheduleStateRevision{}, err
	}
	revision, err := r.client.LoadState(ctx, stateID)
	if err != nil {
		return scheduleStateRevision{}, fmt.Errorf("engine: reload renewed Temporal schedule state %q: %w", stateID, err)
	}
	if revision.state.Pending == nil || revision.state.Pending.Owner != renewed.Owner {
		return scheduleStateRevision{}, errScheduleStateConflict
	}
	return revision, nil
}

func (r *ScheduleReconciler) desired(snapshot ScheduleSnapshot) (map[string]desiredSchedule, error) {
	if strings.TrimSpace(snapshot.InstanceID) == "" {
		return nil, errors.New("engine: schedule snapshot instance ID is required")
	}
	if strings.TrimSpace(snapshot.ConfigSHA) == "" {
		return nil, errors.New("engine: schedule snapshot config SHA is required")
	}
	if snapshot.ConfigGeneration == 0 {
		return nil, errors.New("engine: schedule snapshot config generation is required")
	}

	desired := make(map[string]desiredSchedule)
	for _, template := range snapshot.Runs {
		if template.Gaggle == "" || template.WorkflowName == "" {
			return nil, errors.New("engine: scheduled run requires gaggle and workflow name")
		}
		ordinal := 0
		for _, trigger := range template.Spec.Triggers {
			if trigger.Type != apiv1.TriggerSchedule {
				continue
			}
			if strings.TrimSpace(trigger.Schedule) == "" {
				return nil, fmt.Errorf("engine: workflow %q schedule trigger %d has no cron expression", template.WorkflowName, ordinal)
			}

			id := ScheduleID(snapshot.InstanceID, template.Gaggle, template.WorkflowName, ordinal)
			if _, duplicate := desired[id]; duplicate {
				return nil, fmt.Errorf("engine: duplicate Temporal schedule identity %q", id)
			}
			run := template
			run.RunID = ""
			run.TriggerKind = string(journal.TriggerSchedule)
			run.TriggerRef = id
			options, err := r.scheduleOptions(
				snapshot.ConfigSHA,
				snapshot.ConfigGeneration,
				id,
				trigger.Schedule,
				run,
			)
			if err != nil {
				return nil, err
			}
			desired[id] = desiredSchedule{
				options: options,
			}
			ordinal++
		}
	}
	return desired, nil
}

func scheduleOwnedPrefix(instanceID string) string {
	return scheduleIDPrefix + scheduleHash(instanceID)[:32] + "-"
}

func validateScheduleState(state scheduleReconcileState, instanceID, prefix string) error {
	if state.Version != scheduleStateVersion {
		return fmt.Errorf("engine: unsupported Temporal schedule state version %d", state.Version)
	}
	if state.AppliedGeneration == 0 && state.AppliedConfigSHA != "" {
		return errors.New("engine: Temporal schedule state has a SHA without an applied generation")
	}
	seen := make(map[string]bool, len(state.ManagedScheduleIDs))
	for _, id := range state.ManagedScheduleIDs {
		if !strings.HasPrefix(id, prefix) || id == prefix+"state" {
			return fmt.Errorf("engine: Temporal schedule state contains foreign ID %q", id)
		}
		if seen[id] {
			return fmt.Errorf("engine: Temporal schedule state contains duplicate ID %q", id)
		}
		seen[id] = true
	}
	if state.Pending != nil {
		pending := state.Pending.Snapshot
		if pending.InstanceID != instanceID {
			return fmt.Errorf("engine: Temporal schedule state contains pending snapshot for instance %q", pending.InstanceID)
		}
		if pending.ConfigGeneration <= state.AppliedGeneration {
			return fmt.Errorf(
				"engine: pending config generation %d does not advance applied generation %d",
				pending.ConfigGeneration,
				state.AppliedGeneration,
			)
		}
		if state.Pending.Owner != "" && state.Pending.LeaseUntil.IsZero() {
			return errors.New("engine: owned pending Temporal schedule snapshot has no lease")
		}
	}
	return nil
}

func (r *ScheduleReconciler) scheduleOptions(
	configSHA string,
	configGeneration uint64,
	id string,
	cron string,
	run RunInput,
) (client.ScheduleOptions, error) {
	action := &client.ScheduleWorkflowAction{
		ID:        id,
		Workflow:  RunScheduled,
		Args:      []interface{}{run},
		TaskQueue: r.taskQueue,
		RetryPolicy: &sdktemporal.RetryPolicy{
			MaximumAttempts: 1,
		},
	}
	fingerprint, err := json.Marshal(struct {
		ConfigSHA        string
		ConfigGeneration uint64
		ID               string
		Cron             string
		TaskQueue        string
		CatchupWindow    time.Duration
		Overlap          enumspb.ScheduleOverlapPolicy
		RetryAttempts    int32
		Run              RunInput
	}{
		ConfigSHA:        configSHA,
		ConfigGeneration: configGeneration,
		ID:               id,
		Cron:             cron,
		TaskQueue:        r.taskQueue,
		CatchupWindow:    r.catchupWindow,
		Overlap:          enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
		RetryAttempts:    1,
		Run:              run,
	})
	if err != nil {
		return client.ScheduleOptions{}, fmt.Errorf("engine: fingerprint Temporal schedule %q: %w", id, err)
	}
	sum := sha256.Sum256(fingerprint)
	return client.ScheduleOptions{
		ID:            id,
		Spec:          client.ScheduleSpec{CronExpressions: []string{cron}},
		Action:        action,
		Overlap:       enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
		CatchupWindow: r.catchupWindow,
		Note:          fmt.Sprintf("goobers-managed:%d:%s", configGeneration, hex.EncodeToString(sum[:])),
	}, nil
}

func isScheduleNotFound(err error) bool {
	var notFound *serviceerror.NotFound
	return errors.As(err, &notFound) || status.Code(err) == codes.NotFound
}
