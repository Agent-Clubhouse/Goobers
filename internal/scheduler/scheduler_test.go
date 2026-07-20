package scheduler

import (
	"context"
	"strings"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

func flowSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks:    []apiv1.Task{{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "do"}},
	}
}

// fakeStarter records started run ids and reports duplicates as already-running.
type fakeStarter struct {
	mu        sync.Mutex
	started   map[string]int
	lastInput engine.RunInput
	err       error
}

func (f *fakeStarter) Start(_ context.Context, in engine.RunInput) (engine.StartResult, error) {
	if f.err != nil {
		return engine.StartResult{}, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastInput = in
	if f.started == nil {
		f.started = map[string]int{}
	}
	if f.started[in.RunID] > 0 {
		f.started[in.RunID]++
		return engine.StartResult{RunID: in.RunID, AlreadyRunning: true}, nil
	}
	f.started[in.RunID] = 1
	return engine.StartResult{RunID: in.RunID}, nil
}

func (f *fakeStarter) count(runID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started[runID]
}

func testRegistry(t *testing.T) *engine.Registry {
	t.Helper()
	r := engine.NewRegistry()
	if _, err := r.Register("flow", flowSpec()); err != nil {
		t.Fatalf("register: %v", err)
	}
	return r
}

func newScheduler(t *testing.T, cfg Config) *Scheduler {
	t.Helper()
	if cfg.Gaggle == "" {
		cfg.Gaggle = "web"
	}
	if cfg.Registry == nil {
		cfg.Registry = testRegistry(t)
	}
	cfg.Repo = apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	s, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func backlogEvent() Event {
	item := providers.WorkItem{Provider: providers.ProviderGitHub, ID: "42", Title: "add rate limiting", Labels: []string{"goobers"}}
	return Event{WorkflowName: "flow", Item: &item, Reason: "backlog-item", DedupeKey: "github:42"}
}

// TestTriggerFiresStartsRun: a dispatched event starts a run via the engine.
func TestTriggerFiresStartsRun(t *testing.T) {
	st := &fakeStarter{}
	s := newScheduler(t, Config{Starter: st})
	wantRunID := engine.RunID("web", "flow", "github:42")

	d, err := s.Dispatch(context.Background(), backlogEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !d.Started {
		t.Fatalf("expected a run to start, got: %+v", d)
	}
	if d.RunID != wantRunID {
		t.Errorf("RunID = %q, want %q", d.RunID, wantRunID)
	}
	if st.lastInput.Version != 1 {
		t.Errorf("started version = %d, want 1 (pinned)", st.lastInput.Version)
	}
	if st.lastInput.WorkflowDigest == "" {
		t.Error("started input missing pinned workflow digest")
	}
	if st.lastInput.Item == nil || st.lastInput.Item.ID != "42" {
		t.Errorf("started input missing the backlog item: %+v", st.lastInput.Item)
	}
}

// TestReadinessBlocksRun: an unsatisfied readiness condition holds the run.
func TestReadinessBlocksRun(t *testing.T) {
	st := &fakeStarter{}
	runID := engine.RunID("web", "flow", "github:42")
	block := ReadinessFunc{Label: "capacity", Fn: func(context.Context, Event) (bool, string, error) {
		return false, "no idle workers", nil
	}}
	s := newScheduler(t, Config{Starter: st, Readiness: []ReadinessCondition{block}})

	d, err := s.Dispatch(context.Background(), backlogEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if d.Started {
		t.Error("expected the run to be blocked by readiness")
	}
	if !strings.Contains(d.Reason, "capacity") {
		t.Errorf("reason = %q, want it to mention the blocking condition", d.Reason)
	}
	if st.count(runID) != 0 {
		t.Error("starter must not be called when readiness blocks")
	}
}

// TestBacklogClaimIdempotentNoDoubleStart: dispatching the same item twice starts
// exactly one run.
func TestBacklogClaimIdempotentNoDoubleStart(t *testing.T) {
	st := &fakeStarter{}
	s := newScheduler(t, Config{Starter: st})
	ev := backlogEvent()

	d1, err1 := s.Dispatch(context.Background(), ev)
	d2, err2 := s.Dispatch(context.Background(), ev)
	if err1 != nil || err2 != nil {
		t.Fatalf("dispatch errors: %v / %v", err1, err2)
	}
	if !d1.Started {
		t.Error("first dispatch should start the run")
	}
	if d2.Started {
		t.Error("second dispatch must NOT start a duplicate run")
	}
	if !strings.Contains(d2.Reason, "already") {
		t.Errorf("second reason = %q, want it to note already-running", d2.Reason)
	}
	if got := st.count(engine.RunID("web", "flow", "github:42")); got != 2 {
		t.Errorf("starter saw %d attempts; both should target one run id", got)
	}
}

// TestReadinessOrderConcurrency: a concurrency limit at capacity blocks.
func TestReadinessConcurrencyAtCapacity(t *testing.T) {
	st := &fakeStarter{}
	limit := ConcurrencyLimiter{Max: 1, Active: func(string) int { return 1 }}
	s := newScheduler(t, Config{Starter: st, Readiness: []ReadinessCondition{limit}})

	d, err := s.Dispatch(context.Background(), backlogEvent())
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if d.Started {
		t.Error("expected concurrency limit to block the run")
	}
}

// TestDispatchEmitsSchedulerSpan: a scheduler span is recorded per dispatch.
func TestDispatchEmitsSchedulerSpan(t *testing.T) {
	ctx := context.Background()
	exp := telemetry.NewMemoryExporter()
	cl, err := telemetry.New(ctx, telemetry.Config{ServiceName: "sched-test", SpanExporter: exp})
	if err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}
	defer func() { _ = cl.Shutdown(ctx) }()

	s := newScheduler(t, Config{Starter: &fakeStarter{}, Telemetry: cl})
	if _, err := s.Dispatch(ctx, backlogEvent()); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	_ = cl.Flush(ctx)

	found := false
	for _, sp := range exp.Spans() {
		if sp.Name() == "scheduler/evaluate" {
			found = true
			attrs := map[string]string{}
			for _, attr := range sp.Attributes() {
				attrs[string(attr.Key)] = attr.Value.Emit()
			}
			if attrs[telemetry.AttrRunID] == "" ||
				attrs[telemetry.AttrWorkflowVersion] != "1" ||
				attrs[telemetry.AttrWorkflowDigest] == "" {
				t.Errorf("scheduler identity attrs = %#v", attrs)
			}
			if got := sp.SpanContext().TraceID().String(); got != attrs[telemetry.AttrRunID] {
				t.Errorf("scheduler trace id = %q, want run id %q", got, attrs[telemetry.AttrRunID])
			}
		}
	}
	if !found {
		t.Error("expected a scheduler/evaluate span to be recorded")
	}
}

func TestDispatchUnregisteredWorkflowErrors(t *testing.T) {
	s := newScheduler(t, Config{Starter: &fakeStarter{}})
	if _, err := s.Dispatch(context.Background(), Event{WorkflowName: "ghost", DedupeKey: "x"}); err == nil {
		t.Error("expected an error for an unregistered workflow")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Error("expected error when gaggle missing")
	}
	if _, err := New(Config{Gaggle: "web"}); err == nil {
		t.Error("expected error when registry missing")
	}
	if _, err := New(Config{Gaggle: "web", Registry: engine.NewRegistry()}); err == nil {
		t.Error("expected error when starter missing")
	}
}
