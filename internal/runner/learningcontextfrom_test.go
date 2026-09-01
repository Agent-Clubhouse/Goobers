package runner

// #3928: a stage that declares contextFrom must still receive the learning
// episode a repassing gate injected for it.
//
// Every pre-existing learning-episode fixture — the shared-helper suite in
// learninginjection_test.go, the parity rows, the engine replay tests — walks a
// stage with NO contextFrom, which is the one shape where the defect is
// invisible: with an empty source list SelectContextPointers is the identity.
// The flagship implementation lane declares one
// (reference-workflows/gaggles/goobers/workflows/implementation.yaml), so on
// the lane the injection was built for, the correction was dropped before
// dispatch and before admission.
//
// This file walks a real Start() on that shape and asserts the pointer lands in
// the repass's actual invocation envelope, alongside the negative claims that
// stop the fix from being "select everything": a producer this stage did not
// name is still excluded, and its artifacts do not arrive just because an
// episode did.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/learning"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// The producer and the classifier must agree, or the exemption is decorative:
// LearningEpisodePointerName is the ONLY writer of this pointer name (both
// runners call it, and resume.go rebuilds the same shape), so whatever it emits
// has to be exactly what api/v1alpha1 recognizes as system-generated.
func TestLearningEpisodePointerNameIsClassifiedSystemGenerated(t *testing.T) {
	for _, seq := range []uint64{0, 1, 4, 42, 1 << 32, ^uint64(0)} {
		name := LearningEpisodePointerName(seq)
		class, source := apiv1.ClassifyContextPointer(name)
		if class != apiv1.ContextPointerLearningEpisode {
			t.Fatalf("ClassifyContextPointer(%q) = %v, want learning-episode — the producer and the "+
				"contextFrom classifier have drifted, so the injected pointer is filtered out again", name, class)
		}
		if !class.SystemGenerated() || source != "" {
			t.Fatalf("%q: systemGenerated=%t source=%q, want true and empty",
				name, class.SystemGenerated(), source)
		}
		if got := apiv1.SelectContextPointers(
			[]apiv1.ContextPointer{{Name: name}}, []string{"implement", "review"}); len(got) != 1 {
			t.Fatalf("SelectContextPointers dropped %q under a contextFrom filter", name)
		}
	}
}

// contextFromGateMachine puts the flagship implementation lane's shape around
// the parity fixture's mechanics: a DETERMINISTIC subject and an AUTOMATED
// status-equals gate — the generic retry arm the injection lives on, with no
// reviewer anywhere — but with the re-entered stage declaring contextFrom and a
// maintainer minimum, and with a producer OUTSIDE that list ("enrich") so the
// row can assert the filter still filters.
func contextFromGateMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "query-backlog",
		Tasks: []apiv1.Task{
			{
				Name: "query-backlog", Type: apiv1.TaskDeterministic, Goal: "select the item",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next: "enrich",
			},
			{
				Name: "enrich", Type: apiv1.TaskDeterministic, Goal: "provider-derived enrichment",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next: "implement",
			},
			{
				Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff",
				Run:              &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next:             "review",
				MinimumIntegrity: apiv1.IntegrityMaintainer,
				ContextFrom:      []string{"query-backlog", "implement", "review"},
			},
		},
		Gates: []apiv1.Gate{
			{
				Name: "review", Evaluator: apiv1.EvaluatorAutomated,
				Automated: &apiv1.AutomatedGate{Check: "status-equals"},
				Branches: map[string]string{
					"pass": workflow.TerminalComplete,
					"fail": "implement",
				},
			},
		},
	}
	m, err := workflow.Compile(
		workflow.Definition{Name: "context-from-gate-fixture", Version: 1, Spec: spec},
		workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile contextFrom gate machine: %v", err)
	}
	return m
}

// contextFromDeterministic records every envelope, fails "implement" once with
// the retry-classifiable code the generic arm keys on, and has the two upstream
// producers each record one artifact — which is what gives the run an in-scope
// and an out-of-scope producer to tell apart.
type contextFromDeterministic struct {
	rec      ArtifactRecorder
	attempts map[string]int
	envs     []apiv1.InvocationEnvelope
}

func (c *contextFromDeterministic) Run(
	_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun,
) (apiv1.ResultEnvelope, error) {
	c.envs = append(c.envs, env)
	stage := envelopeStage(env)
	c.attempts[stage]++
	if stage != "implement" {
		ref, err := c.rec.RecordArtifact(stage+".json", []byte(`{"stage":"`+stage+`"}`))
		if err != nil {
			return apiv1.ResultEnvelope{}, err
		}
		return apiv1.ResultEnvelope{
			Status: apiv1.ResultSuccess,
			Artifacts: []apiv1.ArtifactPointer{{
				Path: ref.Path, Digest: ref.Digest, Size: ref.Size,
				MediaType: "application/json", Integrity: ref.Integrity,
			}},
		}, nil
	}
	if c.attempts["implement"] == 1 {
		return apiv1.ResultEnvelope{
			Status: apiv1.ResultFailure,
			Error:  &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "3 tests failed"},
		}, nil
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "implemented"}, nil
}

func (c *contextFromDeterministic) envelopesFor(stage string) []apiv1.InvocationEnvelope {
	var out []apiv1.InvocationEnvelope
	for _, env := range c.envs {
		if envelopeStage(env) == stage {
			out = append(out, env)
		}
	}
	return out
}

// envelopeStage is the stage name inside an envelope's "<runID>:<stage>"
// TaskID, which is how a shared executor tells this fixture's stages apart.
func envelopeStage(env apiv1.InvocationEnvelope) string {
	if i := strings.LastIndex(env.TaskID, ":"); i >= 0 {
		return env.TaskID[i+1:]
	}
	return env.TaskID
}

func findPointer(pointers []apiv1.ContextPointer, name string) *apiv1.ContextPointer {
	for i := range pointers {
		if pointers[i].Name == name {
			return &pointers[i]
		}
	}
	return nil
}

// The end-to-end claim, through a real Start(): a repass into a stage that
// declares contextFrom carries the injected episode, that episode resolves to
// the real correction feedback, and the stage is admitted rather than refused —
// selection runs BEFORE ValidateInputIntegrity, so a surviving pointer is a
// GRADED pointer and the maintainer minimum on this stage has to accept the
// derived episode for the lane to keep running at all.
func TestRunnerRepassWithContextFromReceivesLearningEpisode(t *testing.T) {
	const runID = "run-contextfrom-learning"
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	deterministic := &contextFromDeterministic{attempts: map[string]int{}}
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			deterministic.rec = rec
			return deterministic, nil
		},
		Automated:  gate.NewAutomatedEvaluator(),
		Worktrees:  wtMgr,
		RunsDir:    runsDir,
		ScratchDir: filepath.Join(instanceRoot, "scratch"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: contextFromGateMachine(t),
		Gaggle:  "acme-web",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed — a repass that engages with its correction converges", res.Phase)
	}
	implementEnvs := deterministic.envelopesFor("implement")
	if len(implementEnvs) != 2 {
		t.Fatalf("implement dispatches = %d, want 2 (initial + repass)", len(implementEnvs))
	}
	first, repass := implementEnvs[0], implementEnvs[1]

	// PREMISE: the run really did inject an episode, so a green assertion
	// below cannot mean "nothing was injected and nothing was expected".
	annotation := learningAnnotationEvent(t, filepath.Join(runsDir, runID))
	seq := runnerUint64(annotation.Runner["sourceSeq"])
	want := LearningEpisodePointerName(seq)

	if got := findPointer(first.ContextPointers, want); got != nil {
		t.Fatalf("the FIRST implement dispatch carries %q; no gate had evaluated yet", got.Name)
	}
	episode := findPointer(repass.ContextPointers, want)
	if episode == nil {
		t.Fatalf("repass envelope has no %q pointer; got %v — contextFrom discarded the correction the "+
			"gate injected for this very stage", want, pointerNames(repass.ContextPointers))
	}
	if episode.Integrity != apiv1.IntegrityDerived || episode.Artifact == nil ||
		episode.Artifact.Integrity != apiv1.IntegrityDerived {
		t.Fatalf("episode pointer = %+v, want a derived artifact — selection must not alter provenance", episode)
	}
	data, err := episode.Artifact.Resolve(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("resolve episode artifact: %v", err)
	}
	var decoded learning.Episode
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal episode: %v", err)
	}
	if decoded.Schema != learning.EpisodeSchema || decoded.Gate != "review" || decoded.Stage != "implement" {
		t.Fatalf("episode = %+v, want the review gate's correction for implement", decoded)
	}
	if decoded.CorrectionFeedback == "" {
		t.Fatal("episode carries no correction feedback; the pointer would be inert even once delivered")
	}

	// NEGATIVE ISOLATION: "enrich" is a real producer with a real artifact
	// that this stage did NOT name. It must still be excluded — on both
	// dispatches — or the fix has stopped being a filter.
	for i, env := range implementEnvs {
		for _, name := range pointerNames(env.ContextPointers) {
			if strings.HasPrefix(name, "enrich.") {
				t.Fatalf("implement dispatch %d carries %q from a producer outside contextFrom: %v",
					i+1, name, pointerNames(env.ContextPointers))
			}
		}
	}
	if findPointer(repass.ContextPointers, "query-backlog.artifact[0]") == nil {
		t.Fatalf("repass envelope lost the named producer's artifact; got %v",
			pointerNames(repass.ContextPointers))
	}
	// ADMISSION: the repass ran, so the maintainer minimum admitted the
	// derived episode rather than refusing the stage. Assert the refusal is
	// absent explicitly — a refusal would have failed the run, but a future
	// change that makes it non-fatal must not pass silently.
	for _, ev := range readJournalEvents(t, filepath.Join(runsDir, runID)) {
		if ev.Type == journal.EventError && ev.Error != nil &&
			ev.Error.Code == apiv1.IntegrityAdmissionErrorCode {
			t.Fatalf("integrity admission refused the repass: %s", ev.Error.Message)
		}
	}
}

// learningAnnotationEvent returns the run's single learning.episode.injected
// annotation, failing if the fixture stopped injecting.
func learningAnnotationEvent(t *testing.T, runDir string) journal.Event {
	t.Helper()
	var found []journal.Event
	for _, ev := range readJournalEvents(t, runDir) {
		if ev.Type != journal.EventRunnerAnnotation || ev.Runner == nil {
			continue
		}
		if kind, _ := ev.Runner["kind"].(string); kind == LearningEpisodeInjectedKind {
			found = append(found, ev)
		}
	}
	if len(found) != 1 {
		t.Fatalf("learning.episode.injected annotations = %d, want exactly 1 — this fixture exists to "+
			"assert the injected pointer SURVIVES contextFrom, so it must inject exactly once", len(found))
	}
	return found[0]
}
