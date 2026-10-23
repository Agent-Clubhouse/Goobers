package startinput

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/cloud/startpin"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// generationVariant lets two generation digests carry different content.
type generationVariant func(*instance.ConfigSet)

// source counts every Acquire and Release so a leaked pin on any error path
// shows as acquires != releases.
type source struct {
	mu         sync.Mutex
	dir        string
	variants   map[string]generationVariant
	acquires   int
	released   int
	err        error
	releaseErr error
}

func (s *source) Acquire(_ context.Context, digest string) (childworkflow.Generation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return childworkflow.Generation{}, s.err
	}
	// A fresh load per acquire, like the object-store source.
	set, _, err := instance.LoadConfigDir(s.dir)
	if err != nil {
		return childworkflow.Generation{}, err
	}
	if variant := s.variants[digest]; variant != nil {
		variant(set)
	}
	s.acquires++
	return childworkflow.Generation{Directory: s.dir, Set: set, Release: func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.released++
		return s.releaseErr
	}}, nil
}

type fixtureEnv struct {
	src      *source
	resolver *Resolver
	root     string
	cfg      *instance.Config
	reported []error
}

func newFixture(t *testing.T) *fixtureEnv {
	t.Helper()
	root := filepath.Join("..", "..", "..", "reference-workflows")
	cfg, err := instance.LoadConfig(filepath.Join(root, "instance.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fixtureEnv{root: root, cfg: cfg, src: &source{dir: root, variants: map[string]generationVariant{}}}
	f.resolver = &Resolver{
		Generations: f.src, Config: func() *instance.Config { return cfg }, InstanceID: "instance-1",
		Report: func(err error) { f.reported = append(f.reported, err) },
	}
	return f
}

// pins computes the pins a generation digest's content admits.
func (f *fixtureEnv) pins(t *testing.T, digest string) map[string]cloudconfig.Pins {
	t.Helper()
	set, _, err := instance.LoadConfigDir(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if variant := f.src.variants[digest]; variant != nil {
		variant(set)
	}
	pins, err := startpin.Compute(f.root, f.cfg, set)
	if err != nil {
		t.Fatal(err)
	}
	return pins
}

func (f *fixtureEnv) record(t *testing.T, generation, workflow string, mutate func(*startintent.Envelope)) triggerqueue.Record {
	t.Helper()
	pin := f.pins(t, generation)["goobers/"+workflow]
	envelope := startintent.Envelope{
		Kind: startintent.Kind, Request: startintent.Request{Workflow: workflow, Gaggle: "goobers"},
		Target: startintent.Target{Gaggle: "goobers", Workflow: workflow, ConfigGeneration: generation, WorkflowDigest: pin.WorkflowDigest, GooberDigest: pin.GooberDigest},
	}
	if mutate != nil {
		mutate(&envelope)
	}
	raw, err := envelope.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return triggerqueue.Record{ID: "trigger-run-1", RunID: "run-1", State: triggerqueue.Accepted, Payload: raw}
}

func (f *fixtureEnv) balanced(t *testing.T, label string) {
	t.Helper()
	if f.src.acquires != f.src.released {
		t.Fatalf("%s: generation acquires %d != releases %d (leaked reader pin)", label, f.src.acquires, f.src.released)
	}
}

func TestResolveBuildsThePinnedRunInput(t *testing.T) {
	f := newFixture(t)
	in, err := f.resolver.Resolve(t.Context(), f.record(t, "gen-1", "implementation", nil))
	if err != nil {
		t.Fatal(err)
	}
	if in.InstanceID != "instance-1" || in.RunID != "run-1" || in.Gaggle != "goobers" || in.WorkflowName != "implementation" {
		t.Fatalf("identity = %+v", in)
	}
	if in.ConfigGeneration != "gen-1" || in.WorkflowDigest == "" || in.GooberDigest == "" || !in.LiveJournal {
		t.Fatalf("pins = generation %q workflow %q goober %q live %v", in.ConfigGeneration, in.WorkflowDigest, in.GooberDigest, in.LiveJournal)
	}
	if in.RepoRef.Owner == "" || in.Spec.Start == "" || in.DSLVersion == "" {
		t.Fatalf("definition not pinned: repo %+v start %q dsl %q", in.RepoRef, in.Spec.Start, in.DSLVersion)
	}
	if f.src.acquires != 1 {
		t.Fatalf("acquires = %d, want 1", f.src.acquires)
	}
	f.balanced(t, "success")
}

// TestResolveMapsEverySourceToLocalProvenance covers each source kind with the
// local scheduler's exact mapping (SourceTrigger.Trigger): the engine defers
// run-branch provenance for schedule and item, so a wrong kind journals it wrong.
func TestResolveMapsEverySourceToLocalProvenance(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for name, tc := range map[string]struct {
		source   *localscheduler.SourceTrigger
		wantKind string
		wantRef  string
	}{
		"manual":   {nil, "manual", "implementation"},
		"signal":   {&localscheduler.SourceTrigger{Signal: "github-webhook:pull_request", Ref: "pr:7", Webhook: true}, "signal", "pr:7"},
		"schedule": {&localscheduler.SourceTrigger{ScheduledFrom: now, ScheduledAt: now.Add(time.Minute)}, "schedule", "implementation"},
		"worker":   {&localscheduler.SourceTrigger{WorkerKind: "backlog", ObservedAt: now, ObservedCount: 1, WorkerOrdinal: 1}, "item", "implementation"},
	} {
		f := newFixture(t)
		in, err := f.resolver.Resolve(t.Context(), f.record(t, "gen-1", "implementation", func(e *startintent.Envelope) { e.Source = tc.source }))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if in.TriggerKind != tc.wantKind || in.TriggerRef != tc.wantRef {
			t.Errorf("%s: trigger = %q %q, want %q %q", name, in.TriggerKind, in.TriggerRef, tc.wantKind, tc.wantRef)
		}
	}
}

func permanentRefusal(err error) bool { return errors.Is(err, acceptance.ErrPermanentStart) }

func TestEveryRefusalIsClassifiedAndReleasesTheGeneration(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	for name, tc := range map[string]struct {
		record    func() triggerqueue.Record
		permanent bool
		drift     bool
	}{
		"workflow digest drift": {func() triggerqueue.Record {
			return f.record(t, "gen-1", "implementation", func(e *startintent.Envelope) { e.Target.WorkflowDigest = "sha256:other" })
		}, true, true},
		"goober digest drift": {func() triggerqueue.Record {
			return f.record(t, "gen-1", "implementation", func(e *startintent.Envelope) { e.Target.GooberDigest = "sha256:other" })
		}, true, true},
		"engine unsupported definition": {func() triggerqueue.Record { return f.record(t, "gen-1", "quality-sprint", nil) }, true, false},
		"missing workflow": {func() triggerqueue.Record {
			return f.record(t, "gen-1", "implementation", func(e *startintent.Envelope) {
				e.Target.Workflow, e.Request.Workflow = "no-such-workflow", "no-such-workflow"
			})
		}, true, false},
		"missing gaggle": {func() triggerqueue.Record {
			return f.record(t, "gen-1", "implementation", func(e *startintent.Envelope) { e.Target.Gaggle, e.Request.Gaggle = "no-such-gaggle", "no-such-gaggle" })
		}, true, false},
		"unpinned envelope": {func() triggerqueue.Record {
			r := f.record(t, "gen-1", "implementation", nil)
			envelope, err := startintent.Parse(r.Payload)
			if err != nil {
				t.Fatal(err)
			}
			r.Payload = bytes.Replace(r.Payload, []byte(envelope.Target.WorkflowDigest), nil, 1)
			return r
		}, true, false},
		"expired admission deadline": {func() triggerqueue.Record {
			return f.record(t, "gen-1", "implementation", func(e *startintent.Envelope) { e.Deadline = time.Now().Add(-time.Hour) })
		}, true, false},
		"no run identity": {func() triggerqueue.Record {
			r := f.record(t, "gen-1", "implementation", nil)
			r.RunID = ""
			return r
		}, true, false},
	} {
		before := f.src.acquires
		_, err := f.resolver.Resolve(ctx, tc.record())
		if err == nil {
			t.Errorf("%s: resolved", name)
			continue
		}
		if permanentRefusal(err) != tc.permanent {
			t.Errorf("%s: permanent = %v, want %v (%v)", name, permanentRefusal(err), tc.permanent, err)
		}
		if tc.drift && !errors.Is(err, ErrPinDrift) {
			t.Errorf("%s: %v, want ErrPinDrift", name, err)
		}
		_ = before
		f.balanced(t, name)
	}

	// Transient failures are NOT permanent, and still balance.
	f.src.err = errors.New("generation store unreachable")
	if _, err := f.resolver.Resolve(ctx, f.record(t, "gen-1", "implementation", nil)); err == nil || permanentRefusal(err) {
		t.Fatalf("an unreachable generation = %v, want a retryable error", err)
	}
	f.src.err = nil
	f.resolver.Config = func() *instance.Config { return nil }
	if _, err := f.resolver.Resolve(ctx, f.record(t, "gen-1", "implementation", nil)); err == nil || permanentRefusal(err) {
		t.Fatalf("no applied config = %v, want a retryable error", err)
	}
	f.balanced(t, "transient failures")
	if _, err := (&Resolver{}).Resolve(ctx, f.record(t, "gen-1", "implementation", nil)); err == nil {
		t.Fatal("an unwired resolver resolved")
	}
}

func TestResolveReportsAFailedRelease(t *testing.T) {
	f := newFixture(t)
	f.src.releaseErr = errors.New("unpin failed")
	if _, err := f.resolver.Resolve(t.Context(), f.record(t, "gen-1", "implementation", nil)); err != nil {
		t.Fatal(err)
	}
	if len(f.reported) != 1 {
		t.Fatalf("reported = %v, want the failed release surfaced", f.reported)
	}
}

func TestEachGenerationGetsItsOwnCompileCacheEntry(t *testing.T) {
	f := newFixture(t)
	// gen-2 differs in content: a gaggle CI command rewrites its local-ci stages.
	f.src.variants["gen-2"] = func(set *instance.ConfigSet) {
		for i := range set.Gaggles {
			set.Gaggles[i].Spec.CICommand = []string{"make", "verify"}
		}
	}
	first, second := f.pins(t, "gen-1"), f.pins(t, "gen-2")
	var workflow string
	for key, pin := range first {
		if second[key].WorkflowDigest != pin.WorkflowDigest {
			workflow = key[len("goobers/"):]
			break
		}
	}
	if workflow == "" {
		t.Skip("no reference workflow's digest depends on the gaggle CI command")
	}
	for _, generation := range []string{"gen-1", "gen-2", "gen-1", "gen-2"} {
		in, err := f.resolver.Resolve(t.Context(), f.record(t, generation, workflow, nil))
		if err != nil {
			t.Fatalf("%s: %v", generation, err)
		}
		if want := f.pins(t, generation)["goobers/"+workflow]; in.WorkflowDigest != want.WorkflowDigest {
			t.Fatalf("%s resolved digest %q, want its own %q", generation, in.WorkflowDigest, want.WorkflowDigest)
		}
	}
	if len(f.resolver.compiled) != 2 {
		t.Fatalf("compile cache entries = %d, want one per generation", len(f.resolver.compiled))
	}
	f.balanced(t, "two generations")
}

// TestCompileCacheIsInsensitiveToRunnerPolicy pins today's fact: the cache key
// is the archive digest, and the runner fields Compile reads from cfg do not
// move any pin. A future digest input from cfg must fail this test and key the
// cache accordingly.
func TestCompileCacheIsInsensitiveToRunnerPolicy(t *testing.T) {
	f := newFixture(t)
	base := f.pins(t, "gen-1")
	for name, mutate := range map[string]func(*instance.Config){
		"harness command": func(c *instance.Config) { c.Runner.HarnessCommand = map[string][]string{"copilot": {"/other/copilot"}} },
		"env passthrough": func(c *instance.Config) { c.Runner.EnvPassthrough = append(c.Runner.EnvPassthrough, "EXTRA_ENV") },
		"session args":    func(c *instance.Config) { c.Runner.HarnessSessionArgs = map[string][]string{"copilot": {"--x"}} },
	} {
		cfg := *f.cfg
		mutate(&cfg)
		set, _, err := instance.LoadConfigDir(f.root)
		if err != nil {
			t.Fatal(err)
		}
		pins, err := startpin.Compute(f.root, &cfg, set)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for key, pin := range base {
			if pins[key] != pin {
				t.Errorf("%s moved %s: the compile cache key must include runner policy", name, key)
			}
		}
	}
}

func TestResolveCompilesEachGenerationOnce(t *testing.T) {
	f := newFixture(t)
	for range 3 {
		if _, err := f.resolver.Resolve(t.Context(), f.record(t, "gen-1", "implementation", nil)); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.resolver.compiled) != 1 {
		t.Fatalf("compiled generations = %d, want 1", len(f.resolver.compiled))
	}
}

func TestAChildCustodyPayloadIsNotMineAndNeverPermanent(t *testing.T) {
	f := newFixture(t)
	record := f.record(t, "gen-1", "implementation", nil)
	record.Payload = []byte(`{"kind":"goobers.child-start/v1","gaggle":"goobers"}`)
	_, err := f.resolver.Resolve(t.Context(), record)
	if !errors.Is(err, acceptance.ErrNotOrdinaryStart) || permanentRefusal(err) {
		t.Fatalf("a non-start payload = %v, want ErrNotOrdinaryStart and never permanent", err)
	}
	for _, payload := range []string{`not json`, ``, `{}`, `{"kind":""}`} {
		record.Payload = []byte(payload)
		if _, err := f.resolver.Resolve(t.Context(), record); !errors.Is(err, acceptance.ErrCorruptStart) || !errors.Is(err, acceptance.ErrNotOrdinaryStart) || permanentRefusal(err) {
			t.Fatalf("payload %q = %v, want ErrCorruptStart (not-mine, visible, never permanent)", payload, err)
		}
	}
	if f.src.acquires != 0 {
		t.Fatal("a payload that is not a start acquired a generation")
	}
}

// TestTheDeadlineBoundsAdmissionNotRecovery: an accepted row past its deadline
// is refused, but an uncertain dispatching row may already run and must reach
// starter.Start's mine/adopt confirmation.
func TestTheDeadlineBoundsAdmissionNotRecovery(t *testing.T) {
	f := newFixture(t)
	expired := f.record(t, "gen-1", "implementation", func(e *startintent.Envelope) { e.Deadline = time.Now().Add(-time.Hour) })
	if _, err := f.resolver.Resolve(t.Context(), expired); !permanentRefusal(err) {
		t.Fatalf("an expired accepted start = %v, want a permanent refusal", err)
	}
	expired.State = triggerqueue.Dispatching
	if _, err := f.resolver.Resolve(t.Context(), expired); err != nil {
		t.Fatalf("an expired dispatching (uncertain) start = %v, want it resolved for recovery", err)
	}
}
