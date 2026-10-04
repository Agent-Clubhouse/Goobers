package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/api/validate"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/eventexecution"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type eventHostFixture struct {
	layout     instance.Layout
	source     string
	setup      *schedulerSetup
	service    *durableTriggerService
	sched      *localscheduler.Scheduler
	entry      localscheduler.WorkflowEntry
	generation string
	now        time.Time
	wg         sync.WaitGroup
}

func eventHost(t *testing.T) *eventHostFixture { return eventHostConfigured(t, nil) }

func eventHostConfigured(t *testing.T, configure func(*eventHostFixture, string) string) *eventHostFixture {
	t.Helper()
	f := &eventHostFixture{layout: instance.NewLayout(initDeterministicDemo(t)), now: time.Now().UTC()}
	f.source = filepath.Join(f.layout.ConfigDir(), "gaggles/example/workflows/default-implement.yaml")
	source := strings.Replace(deterministicWorkflowYAML, `dslVersion: "2.0"`, `dslVersion: "3.1"`, 1)
	source = strings.Replace(source, "  name: default-implement", "  name: default-implement\n  annotations: {goobers.dev/allow-preview-features: \"true\"}", 1)
	source = strings.Replace(source, "      type: deterministic", "      type: deterministic\n      workspace: scratch", 1)
	if configure != nil {
		source = configure(f, source)
	}
	writeFileContent(t, f.source, source)
	cfg, err := instance.LoadConfig(f.layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	set, report, err := loadConfigDirectory(f.layout.ConfigDir())
	if err != nil {
		t.Fatal(err, report)
	}
	retainer, err := newExecutionGenerationRetainer(f.layout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = retainer.Close() })
	log, _, err := journal.OpenInstanceLog(f.layout.SchedulerDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	registry := newDaemonRunnerRegistry()
	defs, err := buildSchedulerDefinitions(schedulerDefinitionsInput{Layout: f.layout, Config: cfg, Definitions: set, Validation: report, WaitGroup: &f.wg, RunnerRegistry: registry, InstanceLog: log, ProviderQuota: localscheduler.NewProviderQuotaState(), Generations: []*configgeneration.Retainer{retainer}})
	if err != nil {
		t.Fatal(err)
	}
	if len(defs.Entries) < 1 {
		t.Fatalf("entries=%d", len(defs.Entries))
	}
	f.entry = defs.Entries[0]
	for _, entry := range defs.Entries {
		if entry.Workflow == "default-implement" {
			f.entry = entry
		}
	}
	f.entry.Readiness = apiv1.ReadinessConditions{MaxConcurrentRuns: 1, MaxRunsPerHour: 3}
	for i := range defs.Entries {
		if defs.Entries[i].Workflow == f.entry.Workflow {
			defs.Entries[i] = f.entry
		}
	}
	f.sched = localscheduler.New(defs.Entries, log)
	f.service = acceptedService(t, filepath.Join(f.layout.SchedulerDir(), "accepted-triggers.db"), newDaemonTriggerService())
	f.service.dispatch.now = func() time.Time { return f.now }
	f.service.dispatch.AttachScheduler(f.sched)
	f.setup = &schedulerSetup{OrdinaryRuntime: defs.OrdinaryRuntime, Entries: defs.Entries, EngineRuntime: defs.EngineRuntime, RunnerRegistry: registry, EventRuntime: defs.EventRuntime, EventCatalog: defs.EventCatalog, Generations: retainer, SharedRegistry: journal.NewRegistryScrubber()}
	if err = f.setup.installQueuedEvents(f.layout, f.service); err != nil {
		t.Fatal(err)
	}
	if err = f.setup.installEventPublication(f.layout, f.service); err != nil {
		t.Fatal(err)
	}
	f.setup.EventPublisher.service.Now = func() time.Time { return f.now }
	t.Cleanup(f.setup.unregisterEventPublication)
	// Capture gives the same immutable identity retained by normal construction.
	owner, err := f.layout.EnsureIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, f.generation, err = configgeneration.CaptureForInstance(t.Context(), f.layout.ConfigDir(), owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.sched.Wait(); f.wg.Wait() })
	return f
}

func (f *eventHostFixture) accept(t *testing.T, id, mode string) triggerqueue.EventReceipt {
	t.Helper()
	route := eventing.Route{Consumer: "consumer", Revision: "subscription-v1", Workflow: f.entry.Workflow, WorkflowDigest: f.entry.WorkflowDigest, GooberDigest: f.entry.GooberDigest, ConfigGeneration: f.generation}
	if mode != "" {
		route.Debounce = &eventing.Debounce{Key: "all-changes", Window: time.Minute, MaxWait: time.Minute, MaxEvents: 2, InputMode: mode}
	}
	receipt, _, err := f.service.queue.AcceptEvent(t.Context(), triggerqueue.EventAcceptance{Producer: triggerqueue.EventProducer{Gaggle: f.entry.Gaggle, Binding: "test-binding", Actor: "source-" + id}, Envelope: []byte(fmt.Sprintf(`{"specversion":"1.0","source":"/test","id":%q,"type":"changed","data":{"text":"untrusted %s"}}`, id, id)), Plan: eventing.Plan{Revision: "catalog-v1", Routes: []eventing.Route{route}}}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}
func (f *eventHostFixture) record(t *testing.T, receipt triggerqueue.EventReceipt) (triggerqueue.Record, eventing.StartEnvelope) {
	t.Helper()
	deliveries, err := f.service.queue.EventDeliveries(t.Context(), f.entry.Gaggle, receipt.ID)
	if err != nil || len(deliveries) != 1 {
		t.Fatal(deliveries, err)
	}
	record, start, err := f.service.queue.VerifiedEventStart(t.Context(), f.entry.Gaggle, deliveries[0].GroupID)
	if err != nil {
		t.Fatal(err)
	}
	return record, start
}
func (f *eventHostFixture) drain(t *testing.T) {
	t.Helper()
	if err := f.service.Drain(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestEventHostDrainRunsExactArchiveAndSettlesDurableInputs(t *testing.T) {
	for _, mode := range []string{"all", "latest"} {
		t.Run(mode, func(t *testing.T) {
			f := eventHost(t)
			receipt := f.accept(t, "one", mode)
			f.accept(t, "two", mode)
			// The mutable catalog source cannot change the consumer after acceptance.
			writeFileContent(t, f.source, "invalid pending YAML: [")
			for range 3 {
				f.drain(t)
			}
			f.sched.Wait()
			f.wg.Wait()
			f.drain(t)
			f.drain(t)
			record, start := f.record(t, receipt)
			if record.State != triggerqueue.Dispatched {
				t.Fatalf("state=%s reason=%s", record.State, record.Reason)
			}
			dir, err := f.layout.FindRunDir(record.RunID)
			if err != nil {
				t.Fatal(err)
			}
			rd, err := journal.OpenReadOnly(dir)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := rd.Identity()
			if err != nil {
				t.Fatal(err)
			}
			validator, err := validate.New()
			if err != nil {
				t.Fatal(err)
			}
			identityJSON, err := json.Marshal(identity)
			if err != nil {
				t.Fatal(err)
			}
			if err = validator.ValidateJSON(schemas.Journal["run"], identityJSON); err != nil {
				t.Fatal(err)
			}
			if identity.Event == nil || identity.ConfigGeneration != f.generation || identity.WorkflowDigest != f.entry.WorkflowDigest {
				t.Fatal("pins or event lineage lost")
			}
			if err = eventexecution.VerifyIdentity(rd, identity, record, start); err != nil {
				t.Fatal(err)
			}
			observed, outcome, err := f.service.events.Observe(t.Context(), record)
			if err != nil || !observed || outcome != "completed" {
				events, _ := rd.Events()
				t.Fatalf("observed=%v outcome=%s err=%v events=%+v", observed, outcome, err, events)
			}
			producer, err := f.service.events.ConsumerProducer(t.Context(), rd, "workflow", "run:"+identity.RunID, "emit")
			if err != nil || producer.RootID != "" || producer.RootGroupID != start.GroupID || producer.CausationID != start.GroupID || producer.Depth != 1 || producer.RunID != identity.RunID {
				t.Fatalf("causal producer=%+v err=%v", producer, err)
			}
			group, err := f.service.queue.EventGroup(t.Context(), start.Gaggle, start.GroupID)
			if err != nil || group.SettledAt.IsZero() {
				t.Fatalf("group=%+v err=%v", group, err)
			}
			_, bundle, err := eventexecution.Inputs(t.Context(), f.service.queue, start.Gaggle, start.GroupID)
			if err != nil {
				t.Fatal(err)
			}
			selected := 2
			if mode == "latest" {
				selected = 1
			}
			if len(bundle.Envelopes) != selected || len(bundle.Manifest.Members) != 2 || bundle.Manifest.Members[0].Producer.Actor != "source-one" || bundle.Manifest.Members[1].Producer.Actor != "source-two" {
				t.Fatal("selection or producer attribution lost")
			}
			// Reconciliation after reopening custody must not create another journal.
			if err = f.service.queue.Close(); err != nil {
				t.Fatal(err)
			}
			f.service = acceptedService(t, filepath.Join(f.layout.SchedulerDir(), "accepted-triggers.db"), f.service.dispatch)
			if err = f.setup.installQueuedEvents(f.layout, f.service); err != nil {
				t.Fatal(err)
			}
			f.drain(t)
			f.sched.Wait()
			f.wg.Wait()
			entries, err := os.ReadDir(f.layout.ForGaggle(f.entry.Gaggle).RunsDir())
			if err != nil || len(entries) != 1 {
				t.Fatalf("duplicate run: %v %v", entries, err)
			}
		})
	}
}

func TestEventHostCurrentScopeAndCapacityRefuseBeforeEffects(t *testing.T) {
	f := eventHost(t)
	receipt := f.accept(t, "capacity", "")
	release, ok, reason := f.sched.ReserveContinuation("already-running", f.entry.Gaggle, f.entry.Workflow)
	if !ok {
		t.Fatal(reason)
	}
	if err := f.service.Drain(t.Context()); err == nil {
		t.Fatal("capacity refusal missing")
	}
	record, _ := f.record(t, receipt)
	if record.State != triggerqueue.Accepted {
		t.Fatal(record.State)
	}
	release()
	current := f.entry
	current.DisabledReason = "operator disabled"
	if err := f.sched.Reload([]localscheduler.WorkflowEntry{current}, nil, f.now, "before", "disabled"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Drain(t.Context()); err == nil {
		t.Fatal("disabled consumer ran")
	}
	record, _ = f.record(t, receipt)
	if record.State != triggerqueue.Accepted {
		t.Fatal(record.State)
	}
	current = f.entry
	current.RepoRef.Name = "different"
	if err := f.sched.Reload([]localscheduler.WorkflowEntry{current}, nil, f.now, "disabled", "changed"); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Drain(t.Context()); err == nil {
		t.Fatal("changed scope ran")
	}
	if err := f.sched.Reload([]localscheduler.WorkflowEntry{f.entry}, nil, f.now, "changed", "restored"); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	f.sched.Wait()
	f.wg.Wait()
	f.drain(t)
	record, _ = f.record(t, receipt)
	if record.State != triggerqueue.Dispatched {
		t.Fatal(record.State)
	}
}

func TestEventHostRecoveryUsesReservedIdentityAndRefusesChangedPins(t *testing.T) {
	f := eventHost(t)
	receipt := f.accept(t, "recovery", "")
	if _, err := f.service.queue.RouteNextEvent(t.Context(), f.entry.Gaggle, f.now); err != nil {
		t.Fatal(err)
	}
	record, start := f.record(t, receipt)
	_, inputs, err := eventexecution.Inputs(t.Context(), f.service.queue, start.Gaggle, start.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := f.setup.EventRuntime(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Release()
	if err = f.service.queue.BeginDispatch(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	_, err = runtime.runner.Start(t.Context(), runner.StartInput{RunID: strings.TrimPrefix(record.ID, "trigger-"), Gaggle: start.Gaggle, Machine: runtime.machine, GooberDigest: runtime.gooberDigest, RepoRef: runtime.repoRef, Trigger: journal.Trigger{Kind: journal.TriggerSignal, Ref: "event:" + start.GroupID}, EventInputs: &inputs, OnJournalPublished: func() error { return errors.New("handoff interruption") }})
	if err == nil {
		t.Fatal("crash seam ignored")
	}
	dir, err := f.layout.FindRunDir(strings.TrimPrefix(record.ID, "trigger-"))
	if err != nil {
		t.Fatal(err)
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := rd.Identity()
	if err != nil {
		t.Fatal(err)
	}
	f.setup.RunnerRegistry.setGenerationResolver(func(context.Context, journal.RunIdentity) (executionGenerationRuntime, error) {
		t.Fatal("event used named catalog fallback")
		return executionGenerationRuntime{}, nil
	})
	recovered, err := f.setup.RunnerRegistry.executionGeneration(t.Context(), identity)
	if err != nil {
		t.Fatal(err)
	}
	result, err := recovered.runner.Resume(t.Context(), runner.ResumeInput{RunID: identity.RunID, Machine: recovered.machine, GooberDigest: recovered.gooberDigest, RepoRef: recovered.repoRef})
	if err != nil || result.Phase != journal.PhaseCompleted {
		t.Fatal(result, err)
	}
	f.drain(t)
	record, _ = f.record(t, receipt)
	if record.State != triggerqueue.Dispatched || record.RunID != identity.RunID {
		t.Fatal("lost stable run", record)
	}
	identity.Event.ManifestDigest = journal.Digest([]byte("foreign"))
	if _, err = f.setup.RunnerRegistry.executionGeneration(t.Context(), identity); err == nil {
		t.Fatal("foreign manifest resumed")
	}
}

func TestEventHostUncertainAbsenceOnlyRetriesAfterQueueReopen(t *testing.T) {
	f := eventHost(t)
	receipt := f.accept(t, "absent", "")
	if _, err := f.service.queue.RouteNextEvent(t.Context(), f.entry.Gaggle, f.now); err != nil {
		t.Fatal(err)
	}
	record, _ := f.record(t, receipt)
	if err := f.service.queue.BeginDispatch(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	// Within the same process, absence can mean the admitted goroutine has not
	// published yet. It must never be converted back into another start.
	for range 2 {
		f.drain(t)
	}
	pending, _ := f.record(t, receipt)
	if pending.State != triggerqueue.Dispatching {
		t.Fatal(pending.State)
	}
	if err := f.service.queue.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = acceptedService(t, filepath.Join(f.layout.SchedulerDir(), "accepted-triggers.db"), f.service.dispatch)
	if err := f.setup.installQueuedEvents(f.layout, f.service); err != nil {
		t.Fatal(err)
	}
	f.drain(t)
	f.sched.Wait()
	f.wg.Wait()
	for range 2 {
		f.drain(t)
	}
	pending, _ = f.record(t, receipt)
	if pending.State != triggerqueue.Dispatched || pending.RunID != strings.TrimPrefix(record.ID, "trigger-") {
		t.Fatal(pending)
	}
}

func TestEventHostUnfinishedJournalRetainsGroupAndInputPins(t *testing.T) {
	f := eventHost(t)
	receipt := f.accept(t, "human", "")
	if _, err := f.service.queue.RouteNextEvent(t.Context(), f.entry.Gaggle, f.now); err != nil {
		t.Fatal(err)
	}
	record, start := f.record(t, receipt)
	_, inputs, err := eventexecution.Inputs(t.Context(), f.service.queue, start.Gaggle, start.GroupID)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := f.setup.EventRuntime(t.Context(), start)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Release()
	if err = f.service.queue.BeginDispatch(t.Context(), record.ID); err != nil {
		t.Fatal(err)
	}
	_, err = runtime.runner.Start(t.Context(), runner.StartInput{RunID: strings.TrimPrefix(record.ID, "trigger-"), Gaggle: start.Gaggle, Machine: runtime.machine, GooberDigest: runtime.gooberDigest, RepoRef: runtime.repoRef, Trigger: journal.Trigger{Kind: journal.TriggerSignal, Ref: "event:" + start.GroupID}, EventInputs: &inputs, OnJournalPublished: func() error { return errors.New("stop before stages") }})
	if err == nil {
		t.Fatal("publication seam did not interrupt")
	}
	for range 2 {
		f.drain(t)
	}
	group, err := f.service.queue.EventGroup(t.Context(), start.Gaggle, start.GroupID)
	if err != nil || !group.SettledAt.IsZero() {
		t.Fatalf("unfinished input settled: %+v %v", group, err)
	}
	pins, err := eventexecution.RetainedDependencies(t.Context(), f.service.queue)
	if err != nil || !pins.Generations[f.generation] || !pins.Runs[eventexecution.RunRef{Gaggle: start.Gaggle, RunID: strings.TrimPrefix(record.ID, "trigger-")}] {
		t.Fatalf("unfinished dependencies released: %+v %v", pins, err)
	}
}
