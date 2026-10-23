package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/temporaltest"
)

// executeForProjection runs one engine fixture in the Temporal test
// environment and returns its queried journal projection. wantWorkflowErr
// tolerates a failed workflow — a failed run still projects (the projection is
// a function of history, not of success).
func executeForProjection(t *testing.T, in RunInput, acts *Activities, wantWorkflowErr bool) JournalProjection {
	t.Helper()
	var ts testsuite.WorkflowTestSuite
	env := temporaltest.NewWorkflowEnvironment(&ts)
	// Pin the mock clock so two executions of the same fixture replay the
	// same deterministic timeline (the projection's op times come from
	// workflow.Now).
	env.SetStartTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	env.RegisterActivity(acts)
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); (err != nil) != wantWorkflowErr {
		t.Fatalf("workflow error = %v, wantWorkflowErr = %t", err, wantWorkflowErr)
	}
	val, err := env.QueryWorkflow(JournalQuery)
	if err != nil {
		t.Fatalf("query projection: %v", err)
	}
	var proj JournalProjection
	if err := val.Get(&proj); err != nil {
		t.Fatalf("decode projection: %v", err)
	}
	return proj
}

func projectionInput(name string, spec apiv1.WorkflowSpec) RunInput {
	in := runInput(name, spec)
	in.TriggerKind = string(journal.TriggerManual)
	return in
}

func TestProjectionJournalsTypedIntegrityRefusal(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement",
			MinimumIntegrity: apiv1.IntegrityMaintainer,
			Run: &apiv1.DeterministicRun{
				Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch,
			},
		}},
	}
	in := projectionInput("integrity-refusal", spec)
	in.Item = &apiv1.BacklogItem{
		ID: "42", Provider: apiv1.ProviderGitHub, Integrity: apiv1.IntegrityUnapproved,
	}
	projection := executeForProjection(t, in, &Activities{}, true)

	var refusal *journal.Event
	for _, op := range projection.Ops {
		if op.Event == nil {
			continue
		}
		if op.Event.Type == journal.EventStageStarted {
			t.Fatalf("stage dispatched despite integrity refusal: %+v", op.Event)
		}
		if op.Event.Error != nil && op.Event.Error.Code == apiv1.IntegrityAdmissionErrorCode {
			refusal = op.Event
		}
	}
	if refusal == nil || refusal.Integrity != apiv1.IntegrityUnapproved ||
		refusal.MinimumIntegrity != apiv1.IntegrityMaintainer {
		t.Fatalf("typed refusal = %+v", refusal)
	}
}

func TestProjectionDefaultsMissingItemIntegrityConservatively(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle: "web", Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}}, Start: "implement",
		Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskDeterministic, Goal: "implement",
			MinimumIntegrity: apiv1.IntegrityUnapproved,
			Run:              &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
		}},
	}
	in := projectionInput("integrity-normalization", spec)
	in.Item = &apiv1.BacklogItem{
		ID: "42", Provider: apiv1.ProviderGitHub, Labels: []string{"goobers:approved"},
	}
	var invocationIntegrity apiv1.Integrity
	projection := executeForProjection(t, in, &Activities{
		Det: &fakeRunner{run: func(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
			invocationIntegrity = env.Item.Integrity
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		}},
		Workspaces: testWorkspaces(t),
	}, false)
	if invocationIntegrity != apiv1.IntegrityUnapproved {
		t.Fatalf("invocation item integrity = %q, want unapproved", invocationIntegrity)
	}
	if projection.Item == nil || projection.Item.Integrity != apiv1.IntegrityUnapproved {
		t.Fatalf("projected item = %+v, want unapproved integrity", projection.Item)
	}

}

// readDirBytes reads every regular file under dir into a path→content map, so
// two projected journals can be compared byte-for-byte.

// TestProjectionOpTimesAreWorkflowClock guards against wall-clock leakage: op
// times come from workflow.Now, so replaying the same fixture yields the same
// projection (and therefore the same journal bytes).
func TestProjectionOpTimesAreWorkflowClock(t *testing.T) {
	spec := crSpec("implement", []apiv1.Task{crTask("implement", "")}, nil)
	newProj := func() JournalProjection {
		return executeForProjection(t, projectionInput("proj-clock", spec), &Activities{
			Det:        &scriptedStages{},
			Workspaces: testWorkspaces(t),
		}, false)
	}
	a, b := newProj(), newProj()
	if len(a.Ops) != len(b.Ops) {
		t.Fatalf("op counts differ: %d vs %d", len(a.Ops), len(b.Ops))
	}
	for i := range a.Ops {
		if !a.Ops[i].Time.Equal(b.Ops[i].Time) {
			t.Errorf("op %d time differs across identical executions: %v vs %v", i, a.Ops[i].Time, b.Ops[i].Time)
		}
	}
	var zero time.Time
	for i, op := range a.Ops {
		if op.Time.Equal(zero) {
			t.Errorf("op %d has a zero time", i)
		}
	}
}

type mutationSidecarDeterministic struct {
	line   string
	status apiv1.ResultStatus
}

func (d mutationSidecarDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if err := os.WriteFile(filepath.Join(env.Workspace, mutationsSidecarFile), []byte(d.line+"\n"), 0o644); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	status := d.status
	if status == "" {
		status = apiv1.ResultSuccess
	}
	return apiv1.ResultEnvelope{Status: status}, nil
}

func TestProjectionCarriesProviderMutationProvenance(t *testing.T) {
	spec := crSpec("mutate", []apiv1.Task{crTask("mutate", "")}, nil)
	spec.Tasks[0].Run.Workspace = apiv1.WorkspaceRepo
	in := projectionInput("mutation-provenance", spec)
	in.InstanceID = "e62c1c105fdc4273a72d199394b41cb0"
	proj := executeForProjection(t, in, &Activities{
		Det: mutationSidecarDeterministic{
			line:   `{"provider":"github","kind":"issue","id":"629","url":"https://github.com/Agent-Clubhouse/Goobers/issues/629","operation":"update"}`,
			status: apiv1.ResultNoWork,
		},
		Workspaces: testWorkspaces(t),
	}, false)
	if proj.Identity.InstanceID != in.InstanceID {
		t.Fatalf("engine journal lost instance identity: %q", proj.Identity.InstanceID)
	}

	var branch, mutation *journal.Event
	for _, op := range proj.Ops {
		if op.Event == nil || op.Event.Type != journal.EventRefTouched {
			continue
		}
		if op.Event.Stage == "mutate" {
			mutation = op.Event
		} else if op.Event.ExternalRef != nil && op.Event.ExternalRef.Kind == "branch" {
			branch = op.Event
		}
	}
	if branch == nil {
		t.Fatal("mutating no-work result did not record run-branch provenance")
	}
	if mutation == nil || mutation.ExternalRef == nil {
		t.Fatalf("projected ops carry no stage mutation: %+v", proj.Ops)
	}
	if got := *mutation.ExternalRef; got.Provider != "github" || got.Kind != "issue" || got.ID != "629" {
		t.Fatalf("mutation ref = %+v", got)
	}
	if mutation.Runner["operation"] != "update" {
		t.Fatalf("mutation runner annotation = %+v", mutation.Runner)
	}
}

func TestProjectionMalformedMutationProvenanceIsNonFatal(t *testing.T) {
	spec := crSpec("mutate", []apiv1.Task{crTask("mutate", "")}, nil)
	proj := executeForProjection(t, projectionInput("invalid-mutation", spec), &Activities{
		Det:        mutationSidecarDeterministic{line: `{"provider":"github","kind":"issue"}`},
		Workspaces: testWorkspaces(t),
	}, false)

	var issue *journal.Event
	for _, op := range proj.Ops {
		if op.Event == nil {
			continue
		}
		if op.Event.Type == journal.EventRefTouched && op.Event.Stage == "mutate" {
			t.Fatalf("malformed mutation fabricated provenance: %+v", op.Event)
		}
		if op.Event.Type == journal.EventError && op.Event.Error != nil &&
			op.Event.Error.Code == "mutation_sidecar_read_failed" {
			issue = op.Event
		}
	}
	if issue == nil || issue.Stage != "mutate" {
		t.Fatalf("malformed mutation issue was not surfaced: %+v", proj.Ops)
	}
	last := proj.Ops[len(proj.Ops)-1].Event
	if last == nil || last.Type != journal.EventRunFinished || last.Status != string(journal.PhaseCompleted) {
		t.Fatalf("malformed provenance changed the run outcome: %+v", last)
	}
}

func TestProjectionScrubsVerdictBeforeHistoryAndPointerAddressing(t *testing.T) {
	const secret = "history-secret-value"
	registry := journal.NewRegistryScrubber()
	registry.Register([]byte(secret))

	var mu sync.Mutex
	var implementEnvs []apiv1.InvocationEnvelope
	reviews := 0
	inv := &fakeInvoker{
		invoke: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
			mu.Lock()
			implementEnvs = append(implementEnvs, env)
			mu.Unlock()
			return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
		},
		review: func(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
			mu.Lock()
			defer mu.Unlock()
			reviews++
			if reviews == 1 {
				return apiv1.Verdict{Decision: apiv1.VerdictNeedsChanges, Summary: "remove " + secret}, nil
			}
			return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
		},
	}
	proj := executeForProjection(t, projectionInput("scrubbed-verdict", gatedSpec()), &Activities{
		Goober: inv, Workspaces: testWorkspaces(t), Scrubber: registry,
	}, false)
	history, err := json.Marshal(proj)
	if err != nil {
		t.Fatalf("marshal projection: %v", err)
	}
	if strings.Contains(string(history), secret) {
		t.Fatalf("secret survived in Temporal-derived projection history: %s", history)
	}

	mu.Lock()
	repass := append([]apiv1.InvocationEnvelope(nil), implementEnvs...)
	mu.Unlock()
	if len(repass) != 2 {
		t.Fatalf("implement dispatches = %d, want one repass", len(repass))
	}
	pointer := findContextPointer(repass[1].ContextPointers, "review.verdict")
	if pointer == nil || pointer.Artifact == nil {
		t.Fatalf("repass context = %+v, want verdict artifact", repass[1].ContextPointers)
	}
	dir, err := writeTestJournal(t, filepath.Join(t.TempDir(), "runs"), proj)
	if err != nil {
		t.Fatalf("write live journal: %v", err)
	}
	data, err := pointer.Artifact.Resolve(dir)
	if err != nil {
		t.Fatalf("resolve scrubbed verdict pointer: %v", err)
	}
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), journal.Redacted) {
		t.Fatalf("projected verdict was not scrubbed: %s", data)
	}
}
