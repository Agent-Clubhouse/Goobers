package childworkflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/triggerqueue"
)

const engineFreezeGolden = "testdata/engine-freeze.golden.json"

// freezeFixture is one accepted engine child: the pinned context, its start
// envelope as AcceptChild retained it, the lineage and the handoff request.
type freezeFixture struct {
	pinned  EngineFreezeContext
	payload []byte
	source  []byte
	child   triggerqueue.ChildRecord
	request journal.ChildHandoffRequest
}

func engineAdmission() AdmissionContext {
	context := testContext()
	context.Backend = BackendEngine
	context.ConfigDigest = journal.Digest([]byte("pinned config"))
	return context
}

func newFreezeFixture(t *testing.T) freezeFixture {
	t.Helper()
	admission := engineAdmission()
	source := []byte(validProposal)
	proposal, err := validator(t, admission).Validate(source)
	if err != nil {
		t.Fatal(err)
	}
	occurrence := journal.StageAttemptID("parent-run", 0, "plan", 5)
	execution := ExecutionBinding{InstanceID: "instance-a", RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"}, BranchNamespace: "goobers"}
	authority := Authority{
		Execution:        &execution,
		Origin:           Origin{Gaggle: "web", RunID: "parent-run", StageOccurrence: occurrence, AttemptID: occurrence},
		Admission:        admission,
		ConfigGeneration: journal.Digest([]byte("config generation")), ParentWorkflow: "parent",
		ParentWorkflowDigest: journal.Digest([]byte("parent workflow")), ParentGooberDigest: journal.Digest([]byte("goober kit")),
	}
	envelope, err := childStartEnvelope(authority, proposal, "call-1")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	child := triggerqueue.ChildRecord{Identity: envelope.Identity(), AcceptanceID: "trigger-child-run-1", RunID: "child-run-1", ProposalDigest: proposal.SourceDigest,
		AcceptedAt: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)}
	request := journal.ChildHandoffRequest{Gaggle: "web", ParentRunID: "parent-run", RequestID: journal.Digest([]byte("request")), Action: "wait", ChildRunID: child.RunID,
		AcceptanceID: child.AcceptanceID, InvocationKey: "call-1", SourceDigest: proposal.SourceDigest, Origin: apiv1.ChildWorkflowOrigin{StageOccurrence: occurrence, AttemptID: occurrence}}
	pinned := EngineFreezeContext{Admission: admission, Execution: execution, ConfigGeneration: authority.ConfigGeneration,
		ParentWorkflowDigest: authority.ParentWorkflowDigest, ParentGooberDigest: authority.ParentGooberDigest}
	return freezeFixture{pinned: pinned, payload: payload, source: source, child: child, request: request}
}

func (f freezeFixture) freezer() EngineFreezer {
	return EngineFreezer{Resolve: func(context.Context, ChildStartEnvelope) (EngineFreezeContext, error) { return f.pinned, nil }}
}

func (f freezeFixture) freeze(t *testing.T) ([]byte, engine.FrozenChildManifest) {
	t.Helper()
	manifest, err := f.freezer().Freeze(t.Context(), f.request, f.child, f.payload, f.source)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	return data, manifest
}

func TestEngineFreezeIsDeterministicAndMatchesGolden(t *testing.T) {
	fixture := newFreezeFixture(t)
	first, _ := fixture.freeze(t)
	// A lease successor re-runs the freezer from the same retained inputs.
	second, _ := newFreezeFixture(t).freeze(t)
	if !bytes.Equal(first, second) {
		t.Fatal("freezing the same accepted child produced different bytes")
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, first, "", "  "); err != nil {
		t.Fatal(err)
	}
	indented.WriteByte('\n')
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(engineFreezeGolden, indented.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	golden, err := os.ReadFile(engineFreezeGolden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(golden, indented.Bytes()) {
		t.Fatalf("frozen manifest changed; a digest-stable encoding change needs a new manifest version (UPDATE_GOLDEN=1 to refresh)\n%s", indented.String())
	}
}

// manifestStore returns exactly the frozen bytes to the engine's own loader.
type manifestStore struct{ data []byte }

func (m manifestStore) Admit(context.Context, engine.ChildAdmissionInput) (engine.ChildRunBinding, error) {
	return engine.ChildRunBinding{}, errors.New("unused")
}
func (m manifestStore) Load(context.Context, engine.ChildRunBinding) ([]byte, error) {
	return m.data, nil
}
func (m manifestStore) Cancel(context.Context, engine.ChildRunBinding) error { return nil }
func (m manifestStore) Park(context.Context, engine.ChildParkInput) error    { return nil }
func (m manifestStore) Finalize(context.Context, engine.ChildFinalizeInput) (childresult.TerminalReturn, error) {
	return childresult.TerminalReturn{}, nil
}
func (m manifestStore) Complete(context.Context, engine.ChildCompletionInput) (apiv1.ContextPointer, error) {
	return apiv1.ContextPointer{}, nil
}

func TestFrozenBytesMatchTheAdmittedDigestThroughTheEngine(t *testing.T) {
	fixture := newFreezeFixture(t)
	data, manifest := fixture.freeze(t)
	binding := engine.ChildRunBinding{Request: fixture.request, Child: fixture.child, ManifestDigest: journal.Digest(data), ParentWorkflow: "parent",
		ParentWorkflowID: "parent-wf", ParentTemporalRunID: "parent-trun", Deadline: fixture.child.AcceptedAt.Add(time.Hour)}
	loaded, err := (&engine.Activities{EngineChildren: manifestStore{data}}).LoadEngineChild(t.Context(), binding)
	if err != nil {
		t.Fatalf("engine refused the freezer's own manifest: %v", err)
	}
	if !reflect.DeepEqual(loaded, manifest.Input) {
		t.Fatalf("engine loaded a different input:\n got %+v\nwant %+v", loaded, manifest.Input)
	}
	tampered := append(append([]byte(nil), data...), ' ')
	if _, err := (&engine.Activities{EngineChildren: manifestStore{tampered}}).LoadEngineChild(t.Context(), binding); err == nil {
		t.Fatal("engine accepted bytes that differ from the admitted digest")
	}
}

func TestEngineFreezeRefusesDrift(t *testing.T) {
	cases := map[string]func(*freezeFixture){
		// Policy is strict until accepted policy bytes are retained (#8079):
		// a narrowing refuses as well as a broadening.
		"narrowed policy": func(f *freezeFixture) {
			f.pinned.Admission.ParentTask.ChildWorkflows.AllowedCapabilities = []string{"agent:model"}
		},
		// A valid capability the parent stage declares, so the request passes
		// validation and only the PolicyDigest check can refuse it.
		"broadened policy": func(f *freezeFixture) {
			f.pinned.Admission.ParentTask.Capabilities = append(f.pinned.Admission.ParentTask.Capabilities, "repo:push")
			f.pinned.Admission.GrantedCapabilities = append(f.pinned.Admission.GrantedCapabilities, "repo:push")
		},
		"broadened publication": func(f *freezeFixture) { f.pinned.Admission.AllowPRPublication = !f.pinned.Admission.AllowPRPublication },
		// The child runs only against the generation its parent was accepted under.
		"other generation":             func(f *freezeFixture) { f.pinned.ConfigGeneration = journal.Digest([]byte("reloaded generation")) },
		"other parent workflow digest": func(f *freezeFixture) { f.pinned.ParentWorkflowDigest = journal.Digest([]byte("edited parent")) },
		"other parent goober digest":   func(f *freezeFixture) { f.pinned.ParentGooberDigest = journal.Digest([]byte("edited goober")) },
		// The bindings decide where the child acts; a resolver cannot move them.
		"other repository":        func(f *freezeFixture) { f.pinned.Execution.RepoRef.Owner = "evil" },
		"other repository branch": func(f *freezeFixture) { f.pinned.Execution.RepoRef.Branch = "release" },
		"other branch namespace":  func(f *freezeFixture) { f.pinned.Execution.BranchNamespace = "other" },
		"other instance":          func(f *freezeFixture) { f.pinned.Execution.InstanceID = "instance-b" },
		"unpinned execution": func(f *freezeFixture) {
			var e ChildStartEnvelope
			_ = json.Unmarshal(f.payload, &e)
			e.ExecutionDigest = ""
			f.payload, _ = json.Marshal(e)
		},
		"runner backend":     func(f *freezeFixture) { f.pinned.Admission.Backend = BackendRunner },
		"other parent stage": func(f *freezeFixture) { f.pinned.Admission.ParentTask.Name = "other" },
		"reformatted source": func(f *freezeFixture) { f.source = append([]byte("# reformatted\n"), f.source...) },
		"other lineage":      func(f *freezeFixture) { f.child.Identity.InvocationKey = "call-2" },
		"other request":      func(f *freezeFixture) { f.request.InvocationKey = "call-2" },
		"runner envelope": func(f *freezeFixture) {
			var e ChildStartEnvelope
			_ = json.Unmarshal(f.payload, &e)
			e.Backend = BackendRunner
			f.payload, _ = json.Marshal(e)
		},
		"compiled identity drift": func(f *freezeFixture) {
			var e ChildStartEnvelope
			_ = json.Unmarshal(f.payload, &e)
			e.WorkflowDigest = journal.Digest([]byte("other workflow"))
			f.payload, _ = json.Marshal(e)
		},
		"placement drift": func(f *freezeFixture) {
			var e ChildStartEnvelope
			_ = json.Unmarshal(f.payload, &e)
			e.PlacementsDigest = journal.Digest([]byte("other placements"))
			f.payload, _ = json.Marshal(e)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := newFreezeFixture(t)
			change(&fixture)
			if _, err := fixture.freezer().Freeze(t.Context(), fixture.request, fixture.child, fixture.payload, fixture.source); err == nil {
				t.Fatal("drifted child froze")
			}
		})
	}
	t.Run("resolver failure", func(t *testing.T) {
		fixture := newFreezeFixture(t)
		failing := EngineFreezer{Resolve: func(context.Context, ChildStartEnvelope) (EngineFreezeContext, error) {
			return EngineFreezeContext{}, errors.New("generation gone")
		}}
		if _, err := failing.Freeze(t.Context(), fixture.request, fixture.child, fixture.payload, fixture.source); err == nil {
			t.Fatal("froze without pinned context")
		}
	})
}

func TestStartEnvelopePinsExecutionOnlyForEngineChildren(t *testing.T) {
	fixture := newFreezeFixture(t)
	engineEnvelope, err := DecodeStartEnvelope(fixture.payload)
	if err != nil || engineEnvelope.ExecutionDigest == "" {
		t.Fatalf("engine envelope = %+v %v", engineEnvelope, err)
	}
	runnerEnvelope := engineEnvelope
	runnerEnvelope.Backend = BackendRunner
	withDigest, _ := json.Marshal(runnerEnvelope)
	if _, err := DecodeStartEnvelope(withDigest); err == nil {
		t.Fatal("runner envelope carried an execution binding")
	}
	runnerEnvelope.ExecutionDigest = ""
	runner, _ := json.Marshal(runnerEnvelope)
	if bytes.Contains(runner, []byte("executionDigest")) {
		t.Fatal("runner envelopes changed their retained encoding")
	}
	if _, err := DecodeStartEnvelope(runner); err != nil {
		t.Fatalf("runner envelope refused: %v", err)
	}
	admission := engineAdmission()
	proposal, err := validator(t, admission).Validate(fixture.source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := childStartEnvelope(Authority{Admission: admission}, proposal, "call-1"); err == nil {
		t.Fatal("engine child accepted without its parent's execution binding")
	}
}

// A control-plane configuration reload between acceptance and freeze changes
// the running instance inputs folded into Admission.ConfigDigest (harnesses,
// runner policy) but not the pinned generation or the child policy. The child
// still freezes, byte-identically, against its pinned generation.
func TestEngineFreezeSurvivesAControlConfigReloadOnItsPinnedGeneration(t *testing.T) {
	want, _ := newFreezeFixture(t).freeze(t)
	fixture := newFreezeFixture(t)
	fixture.pinned.Admission.ConfigDigest = journal.Digest([]byte("reloaded instance config"))
	fixture.pinned.Admission.KnownHarnesses = append(fixture.pinned.Admission.KnownHarnesses, "reloaded-harness")
	got, manifest := fixture.freeze(t)
	if !bytes.Equal(got, want) {
		t.Fatalf("reload changed the frozen manifest:\n got %s\nwant %s", got, want)
	}
	envelope, err := DecodeStartEnvelope(fixture.payload)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Input.ConfigGeneration != envelope.ConfigGeneration || manifest.Input.GooberDigest != envelope.ParentGooberDigest {
		t.Fatalf("child does not run on its pinned generation: %+v", manifest.Input)
	}
}

// Envelopes accepted before the generation rule (N-1) still freeze to the
// same manifest bytes.
func TestEngineFreezeReplaysAnN1EnvelopeToTheGolden(t *testing.T) {
	payload, err := os.ReadFile("testdata/engine-freeze-envelope-n1.json")
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil {
		t.Fatal(err)
	}
	fixture := newFreezeFixture(t)
	if !bytes.Equal(compact.Bytes(), fixture.payload) {
		t.Fatal("start envelope encoding changed from the N-1 fixture")
	}
	fixture.payload = compact.Bytes()
	data, _ := fixture.freeze(t)
	var indented bytes.Buffer
	if err := json.Indent(&indented, data, "", "  "); err != nil {
		t.Fatal(err)
	}
	indented.WriteByte('\n')
	golden, err := os.ReadFile(engineFreezeGolden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(golden, indented.Bytes()) {
		t.Fatal("N-1 envelope froze to different manifest bytes")
	}
}
