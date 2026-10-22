package main

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/goobers/goobers/internal/controlplane/planes"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type acceptedTriggerPayload struct {
	Request   httpapi.TriggerRequest `json:"request"`
	PodScoped bool                   `json:"podScoped"`
	PodRunID  string                 `json:"podRunId"`
}

type durableTriggerService struct {
	*planes.DurableTriggerService
	ordinary      *startintent.Service
	childFamilies *childFamilyLifecycle
	queue         *triggerqueue.Store
	dispatch      *daemonTriggerService
	children      childExecutionLauncher
	observeChild  childStartObserver
	childCursor   string
}

func newDaemonCoordinationServices(layout instance.Layout, dispatch *daemonTriggerService, runners *daemonRunnerRegistry, auditLog *journal.InstanceLog) (*durableTriggerService, *daemonStateService, *daemonCancelService, error) {
	if auditLog == nil {
		return nil, nil, nil, errors.New("trigger dispatch requires an instance audit journal")
	}
	state, err := newDaemonStateService(layout)
	if err != nil {
		return nil, nil, nil, err
	}
	triggers, err := newDurableTriggerService(filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"), dispatch)
	if err != nil {
		return nil, nil, nil, err
	}
	cancels, err := newPersistentDaemonCancelService(layout, runners, auditLog)
	if err != nil {
		_ = triggers.Close()
		return nil, nil, nil, err
	}
	triggers.SetObserver(acceptedTriggerObserver(layout))
	triggers.observeChild = acceptedChildObserver(layout)
	triggers.SetAuditLog(auditLog)
	triggers.childFamilies = &childFamilyLifecycle{layout: layout, queue: triggers.queue, runners: runners}
	cancels.fenceChildren = triggers.childFamilies.Fence
	return triggers, state, cancels, nil
}

func newDurableTriggerService(path string, dispatch *daemonTriggerService) (*durableTriggerService, error) {
	service := &durableTriggerService{dispatch: dispatch}
	core, err := planes.NewDurableTriggerService(path, dispatch, service)
	if err != nil {
		return nil, err
	}
	service.DurableTriggerService = core
	service.queue = core.Queue()
	return service, nil
}

func (s *durableTriggerService) AcceptOrdinary(ctx context.Context, request httpapi.TriggerRequest) (httpapi.TriggerResponse, bool, error) {
	return s.acceptOrdinary(ctx, request)
}
func (s *durableTriggerService) DrainOrdinary(ctx context.Context, record triggerqueue.Record) error {
	return s.drainOrdinary(ctx, record)
}
func (s *durableTriggerService) DrainChild(ctx context.Context, record triggerqueue.Record) error {
	return s.drainChild(ctx, record)
}
func (s *durableTriggerService) HasChildren() bool { return s.children != nil }
func (s *durableTriggerService) SweepChildren(ctx context.Context) error {
	if s.childFamilies == nil {
		return nil
	}
	return s.childFamilies.Sweep(ctx)
}
func (s *durableTriggerService) ReconcileChildren(ctx context.Context) error {
	return s.reconcileChildren(ctx)
}
