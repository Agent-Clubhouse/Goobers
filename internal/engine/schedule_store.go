package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"strconv"
	"strings"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	schedulepb "go.temporal.io/api/schedule/v1"
	"go.temporal.io/api/serviceerror"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	workflowpb "go.temporal.io/api/workflow/v1"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	sdktemporal "go.temporal.io/sdk/temporal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
)

const scheduleStateMemoKey = "goobers.schedule-state.v1"

var errScheduleStateConflict = errors.New("engine: Temporal schedule state changed concurrently")

type scheduleStateRevision struct {
	state       scheduleReconcileState
	token       []byte
	rawSchedule *schedulepb.Schedule
	fakeVersion uint64
}

type scheduleStore interface {
	ApplySchedule(context.Context, client.ScheduleOptions, bool) error
	DeleteSchedule(context.Context, string, uint64) error
	CreateState(context.Context, client.ScheduleOptions, scheduleReconcileState) error
	LoadState(context.Context, string) (scheduleStateRevision, error)
	CompareAndSwapState(context.Context, string, scheduleStateRevision, scheduleReconcileState) error
}

type temporalScheduleStore struct {
	service   workflowservice.WorkflowServiceClient
	namespace string
	converter converter.DataConverter
}

func newTemporalScheduleStore(service workflowservice.WorkflowServiceClient, namespace string) *temporalScheduleStore {
	return &temporalScheduleStore{
		service:   service,
		namespace: namespace,
		converter: converter.GetDefaultDataConverter(),
	}
}

func (s *temporalScheduleStore) ApplySchedule(
	ctx context.Context,
	options client.ScheduleOptions,
	previouslyManaged bool,
) error {
	schedule, err := s.scheduleFromOptions(options)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if previouslyManaged {
			current, err := s.describe(ctx, options.ID)
			if err == nil {
				currentGeneration, managed := managedScheduleGeneration(current.Schedule.GetState().GetNotes())
				desiredGeneration, desiredManaged := managedScheduleGeneration(options.Note)
				if managed && desiredManaged && currentGeneration > desiredGeneration {
					return nil
				}
				if current.Schedule.GetState().GetNotes() == options.Note {
					return nil
				}
				err = s.update(ctx, options.ID, schedule, current.ConflictToken, nil)
				if isScheduleStateConflict(err) {
					return errScheduleStateConflict
				}
				if err != nil {
					return fmt.Errorf("engine: update Temporal schedule %q: %w", options.ID, err)
				}
				return nil
			}
			if !isScheduleNotFound(err) {
				return fmt.Errorf("engine: describe Temporal schedule %q: %w", options.ID, err)
			}
		}

		err := s.create(ctx, options.ID, schedule, nil)
		if isScheduleAlreadyRunning(err) {
			previouslyManaged = true
			continue
		}
		if err != nil {
			return fmt.Errorf("engine: create Temporal schedule %q: %w", options.ID, err)
		}
		return nil
	}
	return errScheduleStateConflict
}

func (s *temporalScheduleStore) DeleteSchedule(ctx context.Context, id string, generation uint64) error {
	current, err := s.describe(ctx, id)
	if err != nil {
		return err
	}
	if currentGeneration, managed := managedScheduleGeneration(current.Schedule.GetState().GetNotes()); managed && currentGeneration > generation {
		return nil
	}
	rpcCtx, cancel := context.WithTimeout(ctx, scheduleRPCTimeout)
	defer cancel()
	_, err = s.service.DeleteSchedule(rpcCtx, &workflowservice.DeleteScheduleRequest{
		Namespace:  s.namespace,
		ScheduleId: id,
		Identity:   "goobers-schedule-reconciler",
	})
	return err
}

func managedScheduleGeneration(note string) (uint64, bool) {
	const prefix = "goobers-managed:"
	if !strings.HasPrefix(note, prefix) {
		return 0, false
	}
	parts := strings.SplitN(strings.TrimPrefix(note, prefix), ":", 2)
	if len(parts) != 2 {
		return 0, false
	}
	generation, err := strconv.ParseUint(parts[0], 10, 64)
	return generation, err == nil
}

func (s *temporalScheduleStore) CreateState(
	ctx context.Context,
	options client.ScheduleOptions,
	state scheduleReconcileState,
) error {
	schedule, err := s.scheduleFromOptions(options)
	if err != nil {
		return err
	}
	memo, err := s.memo(state)
	if err != nil {
		return err
	}
	err = s.create(ctx, options.ID, schedule, memo)
	if isScheduleAlreadyRunning(err) {
		return sdktemporal.ErrScheduleAlreadyRunning
	}
	return err
}

func (s *temporalScheduleStore) LoadState(ctx context.Context, id string) (scheduleStateRevision, error) {
	response, err := s.describe(ctx, id)
	if err != nil {
		return scheduleStateRevision{}, err
	}
	if response.Memo == nil || response.Memo.Fields[scheduleStateMemoKey] == nil {
		return scheduleStateRevision{}, fmt.Errorf("engine: Temporal schedule state %q has no managed-state memo", id)
	}
	var state scheduleReconcileState
	if err := s.converter.FromPayload(response.Memo.Fields[scheduleStateMemoKey], &state); err != nil {
		return scheduleStateRevision{}, fmt.Errorf("engine: decode Temporal schedule state %q: %w", id, err)
	}
	return scheduleStateRevision{
		state:       state,
		token:       append([]byte(nil), response.ConflictToken...),
		rawSchedule: response.Schedule,
	}, nil
}

func (s *temporalScheduleStore) CompareAndSwapState(
	ctx context.Context,
	id string,
	revision scheduleStateRevision,
	state scheduleReconcileState,
) error {
	if revision.rawSchedule == nil {
		return fmt.Errorf("engine: Temporal schedule state %q has no schedule payload", id)
	}
	memo, err := s.memo(state)
	if err != nil {
		return err
	}
	err = s.update(ctx, id, revision.rawSchedule, revision.token, memo)
	if isScheduleStateConflict(err) {
		return errScheduleStateConflict
	}
	if err != nil {
		return fmt.Errorf("engine: update Temporal schedule state %q: %w", id, err)
	}
	return nil
}

func (s *temporalScheduleStore) describe(
	ctx context.Context,
	id string,
) (*workflowservice.DescribeScheduleResponse, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, scheduleRPCTimeout)
	defer cancel()
	return s.service.DescribeSchedule(rpcCtx, &workflowservice.DescribeScheduleRequest{
		Namespace:  s.namespace,
		ScheduleId: id,
	})
}

func (s *temporalScheduleStore) create(
	ctx context.Context,
	id string,
	schedule *schedulepb.Schedule,
	memo *commonpb.Memo,
) error {
	requestID, err := scheduleRequestID()
	if err != nil {
		return err
	}
	rpcCtx, cancel := context.WithTimeout(ctx, scheduleRPCTimeout)
	defer cancel()
	_, err = s.service.CreateSchedule(rpcCtx, &workflowservice.CreateScheduleRequest{
		Namespace:  s.namespace,
		ScheduleId: id,
		Schedule:   schedule,
		Identity:   "goobers-schedule-reconciler",
		RequestId:  requestID,
		Memo:       memo,
	})
	return err
}

func (s *temporalScheduleStore) update(
	ctx context.Context,
	id string,
	schedule *schedulepb.Schedule,
	token []byte,
	memo *commonpb.Memo,
) error {
	requestID, err := scheduleRequestID()
	if err != nil {
		return err
	}
	rpcCtx, cancel := context.WithTimeout(ctx, scheduleRPCTimeout)
	defer cancel()
	_, err = s.service.UpdateSchedule(rpcCtx, &workflowservice.UpdateScheduleRequest{
		Namespace:     s.namespace,
		ScheduleId:    id,
		Schedule:      schedule,
		ConflictToken: token,
		Identity:      "goobers-schedule-reconciler",
		RequestId:     requestID,
		Memo:          memo,
	})
	return err
}

func (s *temporalScheduleStore) memo(state scheduleReconcileState) (*commonpb.Memo, error) {
	payload, err := s.converter.ToPayload(state)
	if err != nil {
		return nil, fmt.Errorf("engine: encode Temporal schedule state: %w", err)
	}
	return &commonpb.Memo{Fields: map[string]*commonpb.Payload{
		scheduleStateMemoKey: payload,
	}}, nil
}

func (s *temporalScheduleStore) scheduleFromOptions(options client.ScheduleOptions) (*schedulepb.Schedule, error) {
	action, ok := options.Action.(*client.ScheduleWorkflowAction)
	if !ok {
		return nil, fmt.Errorf("engine: unsupported Temporal schedule action %T", options.Action)
	}
	if action.RetryPolicy == nil {
		return nil, fmt.Errorf("engine: Temporal schedule %q has no explicit workflow retry policy", options.ID)
	}
	workflowName, err := scheduleWorkflowName(action.Workflow)
	if err != nil {
		return nil, err
	}
	input, err := s.converter.ToPayloads(action.Args...)
	if err != nil {
		return nil, fmt.Errorf("engine: encode Temporal schedule %q input: %w", options.ID, err)
	}
	intervals := make([]*schedulepb.IntervalSpec, 0, len(options.Spec.Intervals))
	for _, interval := range options.Spec.Intervals {
		spec := &schedulepb.IntervalSpec{Interval: durationpb.New(interval.Every)}
		if interval.Offset != 0 {
			spec.Phase = durationpb.New(interval.Offset)
		}
		intervals = append(intervals, spec)
	}
	return &schedulepb.Schedule{
		Spec: &schedulepb.ScheduleSpec{
			CronString: append([]string(nil), options.Spec.CronExpressions...),
			Interval:   intervals,
		},
		Action: &schedulepb.ScheduleAction{Action: &schedulepb.ScheduleAction_StartWorkflow{
			StartWorkflow: &workflowpb.NewWorkflowExecutionInfo{
				WorkflowId:   action.ID,
				WorkflowType: &commonpb.WorkflowType{Name: workflowName},
				TaskQueue: &taskqueuepb.TaskQueue{
					Name: action.TaskQueue,
					Kind: enumspb.TASK_QUEUE_KIND_NORMAL,
				},
				Input: input,
				RetryPolicy: &commonpb.RetryPolicy{
					InitialInterval:        durationpb.New(action.RetryPolicy.InitialInterval),
					BackoffCoefficient:     action.RetryPolicy.BackoffCoefficient,
					MaximumInterval:        durationpb.New(action.RetryPolicy.MaximumInterval),
					MaximumAttempts:        action.RetryPolicy.MaximumAttempts,
					NonRetryableErrorTypes: append([]string(nil), action.RetryPolicy.NonRetryableErrorTypes...),
				},
			},
		}},
		Policies: &schedulepb.SchedulePolicies{
			OverlapPolicy:  options.Overlap,
			CatchupWindow:  durationpb.New(options.CatchupWindow),
			PauseOnFailure: options.PauseOnFailure,
		},
		State: &schedulepb.ScheduleState{
			Notes:            options.Note,
			Paused:           options.Paused,
			LimitedActions:   options.RemainingActions != 0,
			RemainingActions: int64(options.RemainingActions),
		},
	}, nil
}

func scheduleWorkflowName(workflow interface{}) (string, error) {
	if name, ok := workflow.(string); ok {
		return name, nil
	}
	value := reflect.ValueOf(workflow)
	if !value.IsValid() || value.Kind() != reflect.Func {
		return "", fmt.Errorf("engine: invalid Temporal schedule workflow %T", workflow)
	}
	fullName := runtime.FuncForPC(value.Pointer()).Name()
	parts := strings.Split(fullName, ".")
	return strings.TrimSuffix(parts[len(parts)-1], "-fm"), nil
}

func isScheduleStateConflict(err error) bool {
	if err == nil {
		return false
	}
	var failedPrecondition *serviceerror.FailedPrecondition
	return errors.As(err, &failedPrecondition) ||
		status.Code(err) == codes.Aborted ||
		status.Code(err) == codes.FailedPrecondition
}

func isScheduleAlreadyRunning(err error) bool {
	if err == nil {
		return false
	}
	var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
	return errors.As(err, &alreadyStarted) ||
		status.Code(err) == codes.AlreadyExists
}

func scheduleRequestID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("engine: generate Temporal schedule update request ID: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}
