package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

func (s *schedulerSetup) installOrdinaryStarts(layout instance.Layout, triggers *durableTriggerService) error {
	if s.Generations == nil || s.OrdinaryRuntime == nil || triggers == nil {
		return errors.New("ordinary durable admission unavailable")
	}
	catalog := &ordinaryStartCatalog{generation: s.EventCatalog.generation, entries: append([]localscheduler.WorkflowEntry(nil), s.Entries...), store: s.Generations.Store}
	s.OrdinaryCatalog = catalog
	s.SourceStarts = &startintent.Sources{Queue: triggers.queue, Acquire: func(ctx context.Context, target startintent.Target) (func(), error) {
		_, lease, err := s.Generations.Store.Acquire(ctx, target.ConfigGeneration)
		if err != nil {
			return nil, err
		}
		return func() { _ = lease.Release() }, nil
	}}
	engine := s.EngineRuntime
	triggers.ordinary = &startintent.Service{Queue: triggers.queue, Capture: catalog.capture, Build: func(ctx context.Context, target startintent.Target) (startintent.Prepared, error) {
		return s.OrdinaryRuntime(ctx, target, engine)
	}, Scheduler: func() *localscheduler.Scheduler {
		if triggers.dispatch.schedulerReady != nil && !triggers.dispatch.schedulerReady() {
			return nil
		}
		return triggers.dispatch.sched.Load()
	}, RunDirectory: func(ctx context.Context, runID string) (string, error) {
		return acceptedTriggerJournalDir(ctx, layout, runID)
	}, Now: triggers.dispatch.now}
	return nil
}

func ordinaryRequest(r httpapi.TriggerRequest) startintent.Request {
	return startintent.Request{Workflow: r.Workflow, Gaggle: r.Gaggle, SourceRun: r.SourceRun, Force: r.Force, PodScoped: r.PodScoped, PodRunID: r.PodRunID}
}

// Existing accepted legacy requests remain readable and keep their original
// identity on retry. Newly captured requests always use the typed pinned path.
func (s *durableTriggerService) acceptOrdinary(ctx context.Context, r httpapi.TriggerRequest) (httpapi.TriggerResponse, bool, error) {
	if s.ordinary == nil {
		return httpapi.TriggerResponse{}, false, nil
	}
	prior, err := s.queue.ByKey(ctx, r.RequestID)
	if err == nil {
		var header struct {
			Kind string `json:"kind"`
		}
		if err = json.Unmarshal(prior.Payload, &header); err != nil {
			return httpapi.TriggerResponse{}, true, err
		}
		if header.Kind == "" {
			return httpapi.TriggerResponse{}, false, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return httpapi.TriggerResponse{}, true, err
	}
	record, duplicate, err := s.ordinary.Accept(ctx, r.RequestID, r.Actor, ordinaryRequest(r))
	return ordinaryResponse(record, duplicate), true, ordinaryAcceptanceError(err)
}

func ordinaryResponse(record triggerqueue.Record, duplicate bool) httpapi.TriggerResponse {
	return httpapi.TriggerResponse{AcceptanceID: record.ID, State: string(record.State), RunID: record.RunID, Duplicate: duplicate}
}

func ordinaryAcceptanceError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, startintent.ErrInvalid) {
		return httpapi.NewInterventionError(http.StatusBadRequest, httpapi.CodeInvalidRequest, err.Error(), nil)
	}
	if errors.Is(err, triggerqueue.ErrConflict) {
		return httpapi.NewInterventionError(http.StatusConflict, "trigger_request_conflict", "request key belongs to another trigger", nil)
	}
	return httpapi.NewInterventionError(http.StatusServiceUnavailable, "trigger_acceptance_unavailable", "trigger could not be acknowledged; retry with the same key", err)
}

func (s *durableTriggerService) drainOrdinary(ctx context.Context, record triggerqueue.Record) error {
	if s.ordinary == nil {
		return nil
	}
	e, err := startintent.Parse(record.Payload)
	if err != nil {
		return err
	}
	r := httpapi.TriggerRequest{Workflow: e.Target.Workflow, Gaggle: e.Target.Gaggle, SourceRun: e.Request.SourceRun, Force: e.Request.Force, Actor: record.Actor, PodScoped: e.Request.PodScoped, PodRunID: e.Request.PodRunID}
	if err = s.dispatch.validateTriggerAuthority(r); err != nil {
		return s.queue.Finish(ctx, record.ID, triggerqueue.Rejected, "", err.Error(), s.dispatch.now())
	}
	if err = s.auditDispatch(record, r); err != nil {
		return err
	}
	admission, cancel := queueBound(ctx)
	defer cancel()
	return s.ordinary.Dispatch(admission, s.dispatch.lifecycleContext(ctx), record)
}

func (s *schedulerSetup) installDurableWorkflowServices(layout instance.Layout, triggers *durableTriggerService) error {
	if err := s.installOrdinaryStarts(layout, triggers); err != nil {
		return err
	}
	if err := s.installQueuedEvents(layout, triggers); err != nil {
		return err
	}
	return s.installEventPublication(layout, triggers)
}
