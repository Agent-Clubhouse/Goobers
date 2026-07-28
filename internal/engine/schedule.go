package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	sdktemporal "go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

const (
	scheduleIDPrefix         = "goobers-"
	maxScheduleCatchupWindow = 24 * time.Hour
)

// ScheduleSnapshot is one atomically-applied config snapshot. Runs are pinned
// engine inputs with RunID unset; the reconciler materializes each schedule
// trigger and Temporal supplies the per-fire workflow ID.
type ScheduleSnapshot struct {
	InstanceID string
	ConfigSHA  string
	Runs       []RunInput
}

type scheduleClient interface {
	Create(context.Context, client.ScheduleOptions) (client.ScheduleHandle, error)
	List(context.Context, client.ScheduleListOptions) (client.ScheduleListIterator, error)
	GetHandle(context.Context, string) client.ScheduleHandle
}

// ScheduleReconciler materializes a config snapshot as Temporal Schedules.
type ScheduleReconciler struct {
	client        scheduleClient
	taskQueue     string
	catchupWindow time.Duration
}

// NewScheduleReconciler constructs a reconciler with explicit schedule policy.
func NewScheduleReconciler(c client.ScheduleClient, taskQueue string, catchupWindow time.Duration) (*ScheduleReconciler, error) {
	if c == nil {
		return nil, errors.New("engine: Temporal schedule client is required")
	}
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
	options  client.ScheduleOptions
	schedule client.Schedule
}

// Reconcile creates and updates desired schedules, then deletes schedules owned
// by this instance that are absent from the same config snapshot.
func (r *ScheduleReconciler) Reconcile(ctx context.Context, snapshot ScheduleSnapshot) error {
	desired, prefix, err := r.desired(snapshot)
	if err != nil {
		return err
	}
	existing, err := r.listOwned(ctx, prefix)
	if err != nil {
		return err
	}

	for id, want := range desired {
		if err := r.ensure(ctx, id, want, existing[id]); err != nil {
			return err
		}
	}
	for id := range existing {
		if _, ok := desired[id]; ok {
			continue
		}
		if err := r.client.GetHandle(ctx, id).Delete(ctx); err != nil && !isScheduleNotFound(err) {
			return fmt.Errorf("engine: delete Temporal schedule %q: %w", id, err)
		}
	}
	return nil
}

func (r *ScheduleReconciler) desired(snapshot ScheduleSnapshot) (map[string]desiredSchedule, string, error) {
	if strings.TrimSpace(snapshot.InstanceID) == "" {
		return nil, "", errors.New("engine: schedule snapshot instance ID is required")
	}
	if strings.TrimSpace(snapshot.ConfigSHA) == "" {
		return nil, "", errors.New("engine: schedule snapshot config SHA is required")
	}

	prefix := scheduleIDPrefix + scheduleHash(snapshot.InstanceID)[:32] + "-"
	desired := make(map[string]desiredSchedule)
	for _, template := range snapshot.Runs {
		if template.Gaggle == "" || template.WorkflowName == "" {
			return nil, "", errors.New("engine: scheduled run requires gaggle and workflow name")
		}
		ordinal := 0
		for _, trigger := range template.Spec.Triggers {
			if trigger.Type != apiv1.TriggerSchedule {
				continue
			}
			if strings.TrimSpace(trigger.Schedule) == "" {
				return nil, "", fmt.Errorf("engine: workflow %q schedule trigger %d has no cron expression", template.WorkflowName, ordinal)
			}

			id := ScheduleID(snapshot.InstanceID, template.Gaggle, template.WorkflowName, ordinal)
			if _, duplicate := desired[id]; duplicate {
				return nil, "", fmt.Errorf("engine: duplicate Temporal schedule identity %q", id)
			}
			run := template
			run.RunID = ""
			run.TriggerKind = string(journal.TriggerSchedule)
			run.TriggerRef = id
			options, err := r.scheduleOptions(snapshot.ConfigSHA, id, trigger.Schedule, run)
			if err != nil {
				return nil, "", err
			}
			desired[id] = desiredSchedule{
				options: options,
				schedule: client.Schedule{
					Action: options.Action,
					Spec:   &options.Spec,
					Policy: &client.SchedulePolicies{
						Overlap:        options.Overlap,
						CatchupWindow:  options.CatchupWindow,
						PauseOnFailure: options.PauseOnFailure,
					},
					State: &client.ScheduleState{Note: options.Note},
				},
			}
			ordinal++
		}
	}
	return desired, prefix, nil
}

func (r *ScheduleReconciler) scheduleOptions(configSHA, id, cron string, run RunInput) (client.ScheduleOptions, error) {
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
		ConfigSHA     string
		ID            string
		Cron          string
		TaskQueue     string
		CatchupWindow time.Duration
		Overlap       enumspb.ScheduleOverlapPolicy
		RetryAttempts int32
		Run           RunInput
	}{
		ConfigSHA:     configSHA,
		ID:            id,
		Cron:          cron,
		TaskQueue:     r.taskQueue,
		CatchupWindow: r.catchupWindow,
		Overlap:       enumspb.SCHEDULE_OVERLAP_POLICY_SKIP,
		RetryAttempts: 1,
		Run:           run,
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
		Note:          "goobers-managed:" + hex.EncodeToString(sum[:]),
	}, nil
}

func (r *ScheduleReconciler) listOwned(ctx context.Context, prefix string) (map[string]bool, error) {
	iter, err := r.client.List(ctx, client.ScheduleListOptions{})
	if err != nil {
		return nil, fmt.Errorf("engine: list Temporal schedules: %w", err)
	}
	out := make(map[string]bool)
	for iter.HasNext() {
		entry, err := iter.Next()
		if err != nil {
			return nil, fmt.Errorf("engine: iterate Temporal schedules: %w", err)
		}
		if strings.HasPrefix(entry.ID, prefix) {
			out[entry.ID] = true
		}
	}
	return out, nil
}

func (r *ScheduleReconciler) update(ctx context.Context, id string, want desiredSchedule) error {
	handle := r.client.GetHandle(ctx, id)
	description, err := handle.Describe(ctx)
	if err != nil {
		return fmt.Errorf("engine: describe Temporal schedule %q: %w", id, err)
	}
	if description.Schedule.State != nil && description.Schedule.State.Note == want.options.Note {
		return nil
	}
	err = handle.Update(ctx, client.ScheduleUpdateOptions{
		DoUpdate: func(input client.ScheduleUpdateInput) (*client.ScheduleUpdate, error) {
			if input.Description.Schedule.State != nil &&
				input.Description.Schedule.State.Note == want.options.Note {
				return nil, sdktemporal.ErrSkipScheduleUpdate
			}
			schedule := want.schedule
			return &client.ScheduleUpdate{Schedule: &schedule}, nil
		},
	})
	if err != nil && !errors.Is(err, sdktemporal.ErrSkipScheduleUpdate) {
		return fmt.Errorf("engine: update Temporal schedule %q: %w", id, err)
	}
	return nil
}

func (r *ScheduleReconciler) ensure(ctx context.Context, id string, want desiredSchedule, listed bool) error {
	if listed {
		err := r.update(ctx, id, want)
		if err == nil {
			return nil
		}
		if !isScheduleNotFound(err) {
			return err
		}
	}
	if _, err := r.client.Create(ctx, want.options); err != nil {
		if !errors.Is(err, sdktemporal.ErrScheduleAlreadyRunning) {
			return fmt.Errorf("engine: create Temporal schedule %q: %w", id, err)
		}
		return r.update(ctx, id, want)
	}
	return nil
}

func isScheduleNotFound(err error) bool {
	var notFound *serviceerror.NotFound
	return errors.As(err, &notFound)
}
