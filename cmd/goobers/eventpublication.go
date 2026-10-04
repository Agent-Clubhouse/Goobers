package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/eventpublication"
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/internal/workflow"
)

type eventPublicationSnapshot struct {
	generation string
	policies   map[string]*apiv1.GaggleEvents
	tasks      map[localscheduler.WorkflowIdentity]map[string]apiv1.Task
	catalogs   map[string]*eventing.Catalog
}

func buildEventPublicationSnapshot(set *instance.ConfigSet, generation string, machines map[localscheduler.WorkflowIdentity]*workflow.Machine, digests map[localscheduler.WorkflowIdentity]string) (eventPublicationSnapshot, error) {
	snapshot := eventPublicationSnapshot{generation: generation, policies: map[string]*apiv1.GaggleEvents{}, tasks: map[localscheduler.WorkflowIdentity]map[string]apiv1.Task{}, catalogs: map[string]*eventing.Catalog{}}
	for _, g := range set.Gaggles {
		if g.Spec.Events == nil || generation == "" {
			continue
		}
		catalog, err := eventing.CompileConfiguredCatalog(g.Name, generation, g.Spec.Events, func(name string) (eventing.TargetPins, error) {
			key := localscheduler.WorkflowIdentity{Gaggle: g.Name, Workflow: name}
			m := machines[key]
			if m == nil {
				return eventing.TargetPins{}, errors.New("event consumer workflow missing")
			}
			return eventing.TargetPins{WorkflowDigest: m.Digest(), GooberDigest: digests[key]}, nil
		})
		if err != nil {
			return snapshot, err
		}
		snapshot.catalogs[g.Name] = catalog
		if g.Spec.Enabled != nil && !*g.Spec.Enabled {
			continue
		}
		snapshot.policies[g.Name] = g.Spec.Events.DeepCopy()
	}
	for key, machine := range machines {
		if machine.Def.Spec.Enabled != nil && !*machine.Def.Spec.Enabled {
			continue
		}
		tasks := map[string]apiv1.Task{}
		for _, task := range machine.Def.Spec.Tasks {
			tasks[task.Name] = *task.DeepCopy()
		}
		snapshot.tasks[key] = tasks
	}
	return snapshot, nil
}

type daemonEventPublisher struct {
	mu       sync.RWMutex
	snapshot eventPublicationSnapshot
	layout   instance.Layout
	service  *eventpublication.Service
}

var daemonEventPublishers sync.Map

func (s *schedulerSetup) installEventPublication(layout instance.Layout, triggers *durableTriggerService) error {
	if s.Generations == nil || triggers == nil || triggers.events == nil {
		return errors.New("event publication retention unavailable")
	}
	publisher := &daemonEventPublisher{snapshot: s.EventCatalog, layout: layout}
	publisher.service = &eventpublication.Service{Queue: triggers.queue, Consumers: triggers.events, Authorize: publisher.authorize, Scrubber: journal.Chain(s.SharedRegistry, journal.NewPatternScrubber())}
	if _, loaded := daemonEventPublishers.LoadOrStore(layout.Root, publisher); loaded {
		return errors.New("event publisher already installed")
	}
	s.EventPublisher = publisher
	return nil
}

func (s *schedulerSetup) unregisterEventPublication() {
	if s.EventPublisher != nil {
		daemonEventPublishers.CompareAndDelete(s.EventPublisher.layout.Root, s.EventPublisher)
	}
}

func eventPublicationFor(root string, rec runner.ArtifactRecorder) executor.EventPublisher {
	return func(ctx context.Context, env apiv1.InvocationEnvelope) (triggerqueue.EventReceipt, error) {
		value, ok := daemonEventPublishers.Load(root)
		if !ok {
			return triggerqueue.EventReceipt{}, errors.New("publish-event requires the local daemon")
		}
		writer, branch, err := runner.OwnedJournalScope(rec)
		if err != nil {
			return triggerqueue.EventReceipt{}, err
		}
		return value.(*daemonEventPublisher).service.Publish(ctx, writer.Dir(), branch, env)
	}
}

func (p *daemonEventPublisher) authorize(ctx context.Context, rd *journal.Reader, id journal.RunIdentity, env apiv1.InvocationEnvelope, eventType string, accept func(*eventing.Catalog) error) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	stage := strings.TrimPrefix(env.TaskID, id.RunID+":")
	task := p.snapshot.tasks[localscheduler.WorkflowIdentity{Gaggle: id.Gaggle, Workflow: id.Workflow}][stage]
	if !publicationTaskAllowed(task) || !eventing.AllowsPublication(p.snapshot.policies[id.Gaggle], id.Workflow, eventType) {
		return errors.New("current workflow event publication permission refused")
	}
	store, err := executionGenerationStore(p.layout)
	if err != nil {
		return err
	}
	directory, lease, err := store.Acquire(ctx, id.ConfigGeneration)
	if err != nil {
		return err
	}
	defer func() { _ = lease.Release() }()
	set, _, err := loadConfigDirectory(directory)
	if err != nil {
		return err
	}
	if !pinnedPublicationAllowed(set, id, eventType) {
		return errors.New("pinned workflow event publication permission refused")
	}
	machine, err := runner.PinnedWorkflowMachine(rd, id)
	if err != nil {
		return err
	}
	pinned, found := machine.Task(stage)
	if !found || machine.Def.DSLVersion != "3.1" || !publicationTaskAllowed(pinned) || env.GooberDigest != id.GooberDigest {
		return errors.New("pinned publication task capability or identity refused")
	}
	// Hold the accepted catalog generation until durable outbox custody takes it.
	_, catalogLease, err := store.Acquire(ctx, p.snapshot.generation)
	if err != nil {
		return err
	}
	defer func() { _ = catalogLease.Release() }()
	return accept(p.snapshot.catalogs[id.Gaggle])
}

func publicationTaskAllowed(task apiv1.Task) bool {
	return task.Type == apiv1.TaskDeterministic && task.Inputs["kind"] == eventing.KindPublishEvent && slices.Contains(task.Capabilities, string(capability.EventPublish))
}

func pinnedPublicationAllowed(set *instance.ConfigSet, id journal.RunIdentity, eventType string) bool {
	for _, g := range set.Gaggles {
		if g.Name == id.Gaggle {
			return (g.Spec.Enabled == nil || *g.Spec.Enabled) && eventing.AllowsPublication(g.Spec.Events, id.Workflow, eventType)
		}
	}
	return false
}

func (r *configReloader) publishEventDefinitions(snapshot eventPublicationSnapshot, publish func() error) error {
	p := r.setup.EventPublisher
	if p == nil {
		return publish()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := publish(); err != nil {
		return err
	}
	p.snapshot = snapshot
	return nil
}
