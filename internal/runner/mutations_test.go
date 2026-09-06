package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

// mutationSidecarDeterministic simulates a provider-chain subcommand
// (backlog-query/open-pr/issue-close-out, issue #228) that recorded a
// mutation fact to the well-known sidecar in its own worktree — a real
// subprocess writes this via cmd/goobers's sidecarMutationRecorder; this
// fake writes it directly to prove the runner-side projection independent of
// the CLI-level plumbing (covered separately in cmd/goobers).
type mutationSidecarDeterministic struct {
	fact   string // one raw JSON line, or "" to write nothing
	status apiv1.ResultStatus
}

func (d mutationSidecarDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	if d.fact != "" {
		if err := os.WriteFile(filepath.Join(env.Workspace, mutationsSidecarFile), []byte(d.fact+"\n"), 0o644); err != nil {
			return apiv1.ResultEnvelope{}, err
		}
	}
	status := d.status
	if status == "" {
		status = apiv1.ResultSuccess
	}
	return apiv1.ResultEnvelope{Status: status, Summary: "mutated"}, nil
}

// TestDispatchTaskProjectsMutationSidecarIntoRefTouched is issue #228's
// headline acceptance: a real open-pr invocation (simulated here at the
// runner level, since the CLI-level negative control lives in cmd/goobers)
// against a fake provider leaves ref.touched{kind:pr} in the run journal.
func TestDispatchTaskProjectsMutationSidecarIntoRefTouched(t *testing.T) {
	machine := fixtureMachine(t)
	fact := `{"provider":"github","kind":"pr","id":"7","url":"https://github.com/acme/web/pull/7","operation":"open"}`
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return mutationSidecarDeterministic{fact: fact}, nil
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-1",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-1"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var found *journal.Event
	for i := range events {
		e := &events[i]
		if e.Type == journal.EventRefTouched && e.ExternalRef != nil && e.ExternalRef.Kind == "pr" {
			found = e
		}
	}
	if found == nil {
		t.Fatalf("expected a ref.touched{kind:pr} event, got: %+v", events)
	}
	if found.ExternalRef.Provider != "github" || found.ExternalRef.ID != "7" || found.ExternalRef.URL != "https://github.com/acme/web/pull/7" {
		t.Fatalf("unexpected ref.touched ExternalRef: %+v", found.ExternalRef)
	}
	if found.Stage != "implement" {
		t.Fatalf("ref.touched Stage = %q, want implement", found.Stage)
	}
	op, _ := found.Runner["operation"].(string)
	if op != "open" {
		t.Fatalf("ref.touched Runner[operation] = %q, want open", op)
	}
}

func TestCommentOnlyRunPersistsPullRequestMutation(t *testing.T) {
	const runID = "run-comment-only"
	machine := fixtureMachine(t)
	fact := `{"provider":"github","kind":"pr","id":"4384","url":"https://github.com/Agent-Clubhouse/Goobers/pull/4384","operation":"comment"}`
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return mutationSidecarDeterministic{fact: fact}, nil
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerItem, Ref: "pr-4384"},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "Agent-Clubhouse", Name: "Goobers", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	db, err := rollup.Open(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatalf("open rollup: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.IngestRun(context.Background(), filepath.Join(runsDir, runID)); err != nil {
		t.Fatalf("IngestRun: %v", err)
	}
	mutations, err := db.ProviderMutations(context.Background(), runID)
	if err != nil {
		t.Fatalf("ProviderMutations: %v", err)
	}
	var pullRequestMutations []rollup.ProviderMutation
	for _, mutation := range mutations {
		if mutation.Kind == "pr" {
			pullRequestMutations = append(pullRequestMutations, mutation)
		}
	}
	if len(pullRequestMutations) != 1 {
		t.Fatalf("ProviderMutations = %#v, want one PR comment mutation", mutations)
	}
	mutation := pullRequestMutations[0]
	if mutation.Provider != "github" || mutation.Kind != "pr" ||
		mutation.ExternalID != "4384" || mutation.Operation != "comment" {
		t.Fatalf("ProviderMutations[0] = %#v, want github PR 4384 comment", mutation)
	}
}

// TestDispatchTaskNoSidecarProjectsNoMutation proves the overwhelmingly
// common case (no sidecar written at all) doesn't fabricate a ref.touched
// event or fail the stage.
func TestDispatchTaskNoSidecarProjectsNoMutation(t *testing.T) {
	machine := fixtureMachine(t)
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return mutationSidecarDeterministic{}, nil
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-1",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-1"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	for _, e := range events {
		if e.Type == journal.EventRefTouched && e.Stage == "implement" {
			t.Fatalf("expected no stage-level ref.touched event without a sidecar, got: %+v", e)
		}
	}
}

func TestDispatchTaskNoWorkWithMutationRetainsProvenance(t *testing.T) {
	machine := noWorkFixtureMachine(t)
	fact := `{"provider":"github","kind":"issue","id":"7","url":"https://github.com/acme/web/issues/7","operation":"update"}`
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return mutationSidecarDeterministic{fact: fact, status: apiv1.ResultNoWork}, nil
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-filtered-mutation",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerItem},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-filtered-mutation"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatal(err)
	}
	var sawBranch, sawMutation bool
	for _, event := range events {
		if event.Type != journal.EventRefTouched || event.ExternalRef == nil {
			continue
		}
		switch event.ExternalRef.Kind {
		case "branch":
			sawBranch = true
		case "issue":
			sawMutation = true
		}
	}
	if !sawBranch || !sawMutation {
		t.Fatalf("mutating no-work tick provenance: branch=%t mutation=%t events=%+v", sawBranch, sawMutation, events)
	}
}

// TestDispatchTaskSurfacesCorruptMutationSidecar is #2029's regression: a
// present-but-unparseable sidecar line must not be silently dropped like
// the benign no-mutations case — it must surface as an EventError, while
// still completing the stage normally, since the provider mutation the
// corrupt line describes already happened for real regardless of whether
// this sidecar can be trusted.
func TestDispatchTaskSurfacesCorruptMutationSidecar(t *testing.T) {
	machine := fixtureMachine(t)
	r, runsDir := newTestRunnerWithDeterministic(t, func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
		return mutationSidecarDeterministic{fact: `{"provider":"github",invalid-json`}, nil
	}, gate.NewAutomatedEvaluator())

	res, err := r.Start(context.Background(), StartInput{
		RunID:   "run-1",
		Machine: machine,
		Gaggle:  "acme-web",
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed — a corrupt sidecar must not fail the stage", res.Phase)
	}

	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-1"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var found *journal.Event
	for i := range events {
		e := &events[i]
		if e.Type == journal.EventError && e.Error != nil && e.Error.Code == "mutation_sidecar_read_failed" {
			found = e
		}
		if e.Type == journal.EventRefTouched && e.Stage == "implement" {
			t.Fatalf("expected no fabricated ref.touched event from a corrupt sidecar line, got: %+v", e)
		}
	}
	if found == nil {
		t.Fatalf("expected an EventError{code:mutation_sidecar_read_failed}, got: %+v", events)
	}
	if found.Stage != "implement" {
		t.Fatalf("EventError Stage = %q, want implement", found.Stage)
	}
}
