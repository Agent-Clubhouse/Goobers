package startinput

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/cloud/startpin"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/startintent"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type source struct {
	dir      string
	cfg      *instance.Config
	acquires int
	released int
	err      error
}

func (s *source) Acquire(_ context.Context, _ string) (childworkflow.Generation, error) {
	s.acquires++
	if s.err != nil {
		return childworkflow.Generation{}, s.err
	}
	// A fresh load per acquire, like the object-store source.
	set, _, err := instance.LoadConfigDir(s.dir)
	if err != nil {
		return childworkflow.Generation{}, err
	}
	return childworkflow.Generation{Directory: s.dir, Set: set, Release: func() error { s.released++; return nil }}, nil
}

func fixture(t *testing.T) (*source, *Resolver, map[string]string, func(workflow string, source *localscheduler.SourceTrigger) triggerqueue.Record) {
	t.Helper()
	root := filepath.Join("..", "..", "..", "reference-workflows")
	cfg, err := instance.LoadConfig(filepath.Join(root, "instance.yaml.example"))
	if err != nil {
		t.Fatal(err)
	}
	set, _, err := instance.LoadConfigDir(root)
	if err != nil {
		t.Fatal(err)
	}
	instance.ApplyGaggleCICommand(set)
	instance.ApplyGaggleOutboxMirror(set)
	pins, err := startpin.Compute(root, cfg, set)
	if err != nil {
		t.Fatal(err)
	}
	digests := map[string]string{}
	for key, pin := range pins {
		digests[key] = pin.WorkflowDigest + "|" + pin.GooberDigest
	}
	src := &source{dir: root, cfg: cfg}
	resolver := &Resolver{Generations: src, Config: func() *instance.Config { return cfg }, InstanceID: "instance-1"}
	record := func(workflow string, trigger *localscheduler.SourceTrigger) triggerqueue.Record {
		pin := pins["goobers/"+workflow]
		raw, err := (startintent.Envelope{
			Kind: startintent.Kind, Request: startintent.Request{Workflow: workflow, Gaggle: "goobers"},
			Target: startintent.Target{Gaggle: "goobers", Workflow: workflow, ConfigGeneration: "gen-1", WorkflowDigest: pin.WorkflowDigest, GooberDigest: pin.GooberDigest},
			Source: trigger,
		}).Marshal()
		if err != nil {
			t.Fatal(err)
		}
		return triggerqueue.Record{ID: "trigger-run-1", RunID: "run-1", Payload: raw}
	}
	return src, resolver, digests, record
}

func TestResolveBuildsThePinnedRunInput(t *testing.T) {
	src, resolver, _, record := fixture(t)
	in, err := resolver.Resolve(t.Context(), record("implementation", nil))
	if err != nil {
		t.Fatal(err)
	}
	if in.InstanceID != "instance-1" || in.RunID != "run-1" || in.Gaggle != "goobers" || in.WorkflowName != "implementation" {
		t.Fatalf("identity = %+v", in)
	}
	if in.ConfigGeneration != "gen-1" || in.WorkflowDigest == "" || in.GooberDigest == "" || !in.LiveJournal {
		t.Fatalf("pins = generation %q workflow %q goober %q live %v", in.ConfigGeneration, in.WorkflowDigest, in.GooberDigest, in.LiveJournal)
	}
	if in.TriggerKind != "manual" || in.TriggerRef != "implementation" {
		t.Fatalf("trigger = %q %q", in.TriggerKind, in.TriggerRef)
	}
	if in.RepoRef.Owner == "" || in.Spec.Start == "" || in.DSLVersion == "" {
		t.Fatalf("definition not pinned: repo %+v start %q dsl %q", in.RepoRef, in.Spec.Start, in.DSLVersion)
	}
	if src.acquires != 1 || src.released != 1 {
		t.Fatalf("generation acquires %d releases %d, want 1 and 1", src.acquires, src.released)
	}
}

func TestResolveMapsWebhookProvenanceToASignal(t *testing.T) {
	_, resolver, _, record := fixture(t)
	in, err := resolver.Resolve(t.Context(), record("implementation", &localscheduler.SourceTrigger{Signal: "github-webhook:pull_request", Ref: "pr:7", Webhook: true}))
	if err != nil {
		t.Fatal(err)
	}
	if in.TriggerKind != "signal" || in.TriggerRef != "pr:7" {
		t.Fatalf("trigger = %q %q, want signal pr:7", in.TriggerKind, in.TriggerRef)
	}
}

func TestResolveRefusesDriftAndIncompleteEnvelopes(t *testing.T) {
	src, resolver, _, record := fixture(t)
	drifted := record("implementation", nil)
	envelope, err := startintent.Parse(drifted.Payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Target.WorkflowDigest = "sha256:other"
	drifted.Payload, _ = envelope.Marshal()
	if _, err := resolver.Resolve(t.Context(), drifted); !errors.Is(err, ErrPinDrift) {
		t.Fatalf("drifted workflow digest = %v, want ErrPinDrift", err)
	}
	envelope.Target.WorkflowDigest = ""
	drifted.Payload, _ = envelope.Marshal()
	if _, err := resolver.Resolve(t.Context(), drifted); err == nil {
		t.Fatal("an unpinned envelope resolved")
	}
	noRun := record("implementation", nil)
	noRun.RunID = ""
	if _, err := resolver.Resolve(t.Context(), noRun); err == nil {
		t.Fatal("an acceptance without a run identity resolved")
	}
	src.err = errors.New("generation pruned")
	if _, err := resolver.Resolve(t.Context(), record("implementation", nil)); err == nil {
		t.Fatal("a missing generation resolved")
	}
	if _, err := (&Resolver{}).Resolve(t.Context(), record("implementation", nil)); err == nil {
		t.Fatal("an unwired resolver resolved")
	}
}

func TestResolveRefusesAnEngineUnsupportedDefinition(t *testing.T) {
	_, resolver, _, record := fixture(t)
	if _, err := resolver.Resolve(t.Context(), record("quality-sprint", nil)); err == nil {
		t.Fatal("a workflow with spec.parallels resolved on the engine-only cloud")
	}
}

func TestResolveCompilesEachGenerationOnce(t *testing.T) {
	_, resolver, _, record := fixture(t)
	for range 3 {
		if _, err := resolver.Resolve(t.Context(), record("implementation", nil)); err != nil {
			t.Fatal(err)
		}
	}
	if len(resolver.compiled) != 1 {
		t.Fatalf("compiled generations = %d, want 1", len(resolver.compiled))
	}
}
