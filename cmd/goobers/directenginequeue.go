package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/enginestartintent"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/temporaldial"
	"github.com/goobers/goobers/internal/triggerqueue"
)

var dialDirectEngine = temporaldial.Dial

func directEngineService(layout instance.Layout, queue *triggerqueue.Store, cfg *instance.Config) *enginestartintent.Service {
	return &enginestartintent.Service{Queue: queue, Capture: func(ctx context.Context, r enginestartintent.Request) (engine.RunInput, func(), error) {
		return pinnedDirectEngineInput(ctx, layout, cfg, r.Gaggle, r.Workflow, r.DedupeKey, r.LiveJournal)
	}, Open: func(ctx context.Context, r enginestartintent.Request) (enginestartintent.Backend, error) {
		tls, dc, err := enginestartintent.Transport(cfg, r)
		if err != nil {
			return nil, err
		}
		client, err := dialDirectEngine(ctx, r.HostPort, r.Namespace, tls, dc)
		if err != nil {
			return nil, err
		}
		return &enginestartintent.TemporalBackend{Client: client, Namespace: r.Namespace, TaskQueue: r.TaskQueue, Converter: dc}, nil
	}}
}

func queueDirectEngineStart(ctx context.Context, layout instance.Layout, cfg *instance.Config, request enginestartintent.Request, stdout, stderr io.Writer) int {
	var err error
	request.Directory, err = os.Getwd()
	if err == nil {
		request.Binding, err = enginestartintent.CredentialBinding(cfg)
	}
	if err != nil {
		pf(stderr, "error: bind direct engine transport: %v\n", err)
		return 2
	}
	queue, err := triggerqueue.Open(filepath.Join(layout.SchedulerDir(), "accepted-triggers.db"))
	if err != nil {
		pf(stderr, "error: open direct engine queue: %v\n", err)
		return 1
	}
	defer func() { _ = queue.Close() }()
	service := directEngineService(layout, queue, cfg)
	record, _, err := service.Accept(ctx, request)
	if err != nil {
		pf(stderr, "error: accept direct engine run: %v\n", err)
		return 1
	}
	if err = service.Dispatch(ctx, record); err != nil {
		pf(stderr, "error: direct engine receipt %s remains retained: %v\n", record.ID, err)
		return 1
	}
	e, err := enginestartintent.Parse(record.Payload)
	if err != nil {
		pf(stderr, "error: read direct engine receipt: %v\n", err)
		return 1
	}
	raw, err := queue.DirectEngineInput(ctx, record.ID)
	if err != nil {
		pf(stderr, "error: read direct engine input: %v\n", err)
		return 1
	}
	in, err := e.Input(raw)
	if err != nil {
		pf(stderr, "error: verify direct engine input: %v\n", err)
		return 1
	}
	pf(stdout, "engine run started: %s (workflow=%s v%d, gaggle=%s, queue=%s)\n", in.RunID, in.WorkflowName, in.Version, in.Gaggle, request.TaskQueue)
	return 0
}

func (s *durableTriggerService) drainDirectEngine(ctx context.Context, record triggerqueue.Record) error {
	if s.directEngine == nil {
		return nil
	}
	bounded, cancel := queueBound(ctx)
	defer cancel()
	err := s.directEngine.Dispatch(bounded, record)
	// Unknown effects are retained for subsequent exact observation; they must
	// not block other accepted workflows or be converted into ordinary starts.
	if errors.Is(err, enginestartintent.ErrUncertain) || errors.Is(err, triggerqueue.ErrTransition) {
		return nil
	}
	return err
}

// One remote observation per daemon sweep bounds frontend impact; a stable ID
// cursor progresses past unavailable targets while ordinary starts keep draining.
func (s *durableTriggerService) sweepDirectEngine(ctx context.Context) error {
	if s.directEngine == nil {
		return nil
	}
	page, err := s.queue.DirectEnginePage(ctx, s.directEngineCursor)
	if err != nil {
		return err
	}
	if len(page) == 0 {
		s.directEngineCursor = ""
		return nil
	}
	s.directEngineCursor = page[0].ID
	return s.drainDirectEngine(ctx, page[0])
}
