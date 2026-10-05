// Package eventpublication owns host-local deterministic event publication.
package eventpublication

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/eventexecution"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// Authorization must hold current policy/catalog and archive leases through fn.
// The callback verifies both pinned and current task capability/type ceilings.
type Authorization func(context.Context, *journal.Reader, journal.RunIdentity, apiv1.InvocationEnvelope, string, func(*eventing.Catalog) error) error

// Service accepts only a journal directory and branch established by the
// runner's OwnedJournalScope. It exposes no ingress transport or model endpoint.
type Service struct {
	Queue     *triggerqueue.Store
	Consumers *eventexecution.Service
	Authorize Authorization
	Scrubber  journal.Scrubber
	Now       func() time.Time
	// Accept may qualify an in-process transport in tests. Nil uses Queue directly.
	Accept func(context.Context, triggerqueue.EventAcceptance, time.Time) (triggerqueue.EventReceipt, bool, error)
}

// Publish validates a live committed attempt, persists intent, admits an event,
// and records its receipt before returning success. It never waits for consumers.
func (s *Service) Publish(ctx context.Context, directory string, branch int, env apiv1.InvocationEnvelope) (triggerqueue.EventReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s == nil || s.Queue == nil || s.Authorize == nil || s.Scrubber == nil {
		return triggerqueue.EventReceipt{}, errors.New("event publication host service unavailable")
	}
	publication, err := eventing.ParsePublication(env.Inputs)
	if err != nil {
		return triggerqueue.EventReceipt{}, err
	}
	rd, err := journal.OpenReadOnly(directory)
	if err != nil {
		return triggerqueue.EventReceipt{}, err
	}
	id, occurrence, err := activeOrigin(rd, branch, env)
	if err != nil {
		return triggerqueue.EventReceipt{}, err
	}
	envelope, err := publication.Envelope(id.Gaggle, id.RunID, occurrence)
	if err != nil {
		return triggerqueue.EventReceipt{}, err
	}
	if !bytes.Equal(envelope.JSON, s.Scrubber.Scrub(envelope.JSON)) {
		return triggerqueue.EventReceipt{}, errors.New("event publication contains redacted data")
	}
	producer, err := s.producer(ctx, rd, id, strings.TrimPrefix(env.TaskID, id.RunID+":"))
	if err != nil {
		return triggerqueue.EventReceipt{}, err
	}
	var receipt triggerqueue.EventReceipt
	err = s.Authorize(ctx, rd, id, env, publication.Type, func(catalog *eventing.Catalog) error {
		if catalog == nil {
			return errors.New("event publication catalog unavailable")
		}
		_, plan, err := catalog.Match(id.Gaggle, envelope.JSON)
		if err != nil {
			return err
		}
		for _, route := range plan.Routes {
			if route.Workflow == id.Workflow {
				return errors.New("same-workflow event subscription requires a separately qualified explicit opt-in")
			}
		}
		now := time.Now().UTC()
		if s.Now != nil {
			now = s.Now()
		}
		intent, _, err := s.Queue.BeginEventPublication(ctx, triggerqueue.EventPublication{ID: envelope.ID, ConfigGeneration: id.ConfigGeneration, Occurrence: occurrence, Acceptance: triggerqueue.EventAcceptance{Producer: producer, Envelope: envelope.JSON, Plan: plan}}, now)
		if err != nil {
			return err
		}
		accept := s.Accept
		if accept == nil {
			accept = s.Queue.AcceptEvent
		}
		receipt, _, err = accept(ctx, intent.Acceptance, now)
		if err != nil {
			return invoke.InfrastructureFailure(fmt.Errorf("event receipt unavailable: %w", err))
		}
		if err = s.Queue.CompleteEventPublication(ctx, intent, receipt); err != nil {
			return invoke.InfrastructureFailure(fmt.Errorf("event receipt acknowledgement unavailable: %w", err))
		}
		return nil
	})
	return receipt, err
}

func activeOrigin(rd *journal.Reader, branch int, env apiv1.InvocationEnvelope) (journal.RunIdentity, string, error) {
	id, err := rd.Identity()
	if err != nil {
		return id, "", err
	}
	if id.Child != nil || id.ContinuedFromRunID != "" || id.EngineDriven() {
		return id, "", errors.New("event publication does not support child, continuation or engine ancestry")
	}
	if id.InstanceID != env.InstanceID || id.RunID != env.RunID || id.Gaggle != env.Gaggle || id.Workflow != env.WorkflowID || id.ConfigGeneration == "" || id.ConfigGeneration != env.ConfigGeneration || branch < 0 || env.Attempt < 1 || !slices.Contains(env.Capabilities, string(capability.EventPublish)) {
		return id, "", errors.New("event publication invocation scope refused")
	}
	events, err := rd.Events()
	if err != nil {
		return id, "", err
	}
	if journal.PhaseFromEvents(events) != journal.PhaseRunning {
		return id, "", errors.New("event publication parent is not running")
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		if e.Branch != branch || env.TaskID != id.RunID+":"+e.Stage {
			continue
		}
		if e.Type == journal.EventStageFinished {
			return id, "", errors.New("event publication stage already finished")
		}
		if e.Type != journal.EventStageStarted {
			continue
		}
		if e.Attempt != int(env.Attempt) {
			return id, "", errors.New("event publication attempt superseded")
		}
		occurrence, err := journal.PublicationOccurrence(e)
		return id, occurrence, err
	}
	return id, "", errors.New("event publication lacks a committed stage start")
}

func (s *Service) producer(ctx context.Context, rd *journal.Reader, id journal.RunIdentity, stage string) (eventing.Producer, error) {
	binding := "workflow:" + id.Workflow
	actor := "workflow:" + id.RunID
	if id.Event != nil {
		if s.Consumers == nil {
			return eventing.Producer{}, errors.New("event consumer ancestry service unavailable")
		}
		return s.Consumers.ConsumerProducer(ctx, rd, binding, actor, stage)
	}
	return eventing.Producer{Gaggle: id.Gaggle, Binding: binding, Actor: actor, RunID: id.RunID, Stage: stage, RootID: id.RunID}, nil
}
