package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

// committingSuccessGoober is an agentic stub that commits real work to the run
// branch and completes successfully — the #3366 shape's first half: three
// implement stages of genuine engineering, all green.
type committingSuccessGoober struct {
	t     *testing.T
	calls int
}

func (g *committingSuccessGoober) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.t.Helper()
	g.calls++
	content := []byte(fmt.Sprintf("implement work, attempt %d\n", g.calls))
	if err := os.WriteFile(filepath.Join(env.Workspace, "impl.txt"), content, 0o644); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	runGit(g.t, env.Workspace, "add", "-A")
	runGit(g.t, env.Workspace, "commit", "-m", "implement the backlog item")
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "implemented"}, nil
}

func (g *committingSuccessGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, errors.New("committingSuccessGoober is not a reviewer")
}

// noCommitSuccessGoober completes successfully without ever committing — a
// stage with no diff must record no unpushed-diff artifact (no noise).
type noCommitSuccessGoober struct{}

func (g *noCommitSuccessGoober) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (g *noCommitSuccessGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{}, errors.New("noCommitSuccessGoober is not a reviewer")
}

// envFaultDeterministic reproduces #3366's trigger class 1: local-ci dies on an
// environmental fault (the live specimen's egress 403), a dispatch error that
// terminates the run after the implement work already happened.
type envFaultDeterministic struct{ calls int }

func (d *envFaultDeterministic) Run(context.Context, apiv1.InvocationEnvelope, apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.calls++
	return apiv1.ResultEnvelope{}, errors.New("proxy egress denied: 403 Forbidden fetching module zip")
}

// unpushedDiffMachine compiles implement (agentic) → local-ci (deterministic)
// → complete, the minimal shape of the shipped implementation workflow's
// validated-then-published pipeline.
func unpushedDiffMachine(t *testing.T) *workflow.Machine {
	t.Helper()
	spec := apiv1.WorkflowSpec{
		Gaggle:   "acme-web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{
				Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "produce a diff",
				Next: "local-ci",
			},
			{
				Name: "local-ci", Type: apiv1.TaskDeterministic, Goal: "run make ci",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
				Next: workflow.TerminalComplete,
			},
		},
	}
	m, err := workflow.Compile(workflow.Definition{Name: "implementation", Version: 1, Spec: spec}, workflow.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile unpushed-diff machine: %v", err)
	}
	return m
}

func newUnpushedDiffRunner(t *testing.T, goober invoke.Goober, localCI invoke.Deterministic, claimedItems func(string) ([]string, error)) (*Runner, string) {
	t.Helper()
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	runsDir := filepath.Join(instanceRoot, "runs")
	fixtureRepo := newFixtureRepo(t)
	r, err := New(Config{
		NewDeterministic: func(ArtifactRecorder, SecretRegistrar) (invoke.Deterministic, error) {
			return localCI, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return goober, nil
		},
		Worktrees:    wtMgr,
		RunsDir:      runsDir,
		RepoCloneURL: func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
		ClaimedItems: claimedItems,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r, runsDir
}

func unpushedDiffStartInput(runID string, m *workflow.Machine, item *apiv1.BacklogItem) StartInput {
	trigger := journal.Trigger{Kind: journal.TriggerManual}
	if item != nil {
		trigger = journal.Trigger{Kind: journal.TriggerItem, Ref: item.ID}
	}
	return StartInput{
		RunID:   runID,
		Machine: m,
		Gaggle:  "acme-web",
		Trigger: trigger,
		Item:    item,
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	}
}

// readUnpushedDiffArtifacts returns the recorded unpushed-diff patch bytes and
// decoded metadata, or ok=false when either artifact is absent.
func readUnpushedDiffArtifacts(t *testing.T, runsDir, runID string) (patch []byte, meta unpushedDiffMetadata, ok bool) {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	var patchRef, metaRef *journal.Ref
	for i := range events {
		e := events[i]
		if e.Type != journal.EventArtifactRecorded || e.Ref == nil {
			continue
		}
		switch {
		case strings.HasSuffix(e.Name, "/"+unpushedDiffPatchName):
			patchRef = e.Ref
		case strings.HasSuffix(e.Name, "/"+unpushedDiffMetaName):
			metaRef = e.Ref
		}
	}
	if patchRef == nil || metaRef == nil {
		return nil, unpushedDiffMetadata{}, false
	}
	patch, err = rd.ArtifactBytes(*patchRef)
	if err != nil {
		t.Fatalf("read patch artifact: %v", err)
	}
	metaBytes, err := rd.ArtifactBytes(*metaRef)
	if err != nil {
		t.Fatalf("read metadata artifact: %v", err)
	}
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		t.Fatalf("unmarshal metadata artifact: %v", err)
	}
	return patch, meta, true
}

// TestEnvironmentalFaultAtLocalCIPreservesImplementDiff is #3366's headline
// regression: implement commits real, validated work; local-ci then dies on an
// environmental fault (the live specimen's egress 403). Before the fix the
// diff died with the worktree — no branch, no PR, no artifact. Now the diff
// and its discovery sidecar are in the journal BEFORE local-ci ever ran, so
// the fault strands recoverable work instead of destroying it.
func TestEnvironmentalFaultAtLocalCIPreservesImplementDiff(t *testing.T) {
	goober := &committingSuccessGoober{t: t}
	localCI := &envFaultDeterministic{}
	r, runsDir := newUnpushedDiffRunner(t, goober, localCI, nil)

	item := &apiv1.BacklogItem{ID: "3366", Provider: apiv1.ProviderGitHub, URL: "https://github.com/acme/web/issues/3366", Integrity: apiv1.IntegrityMaintainer}
	res, err := r.Start(context.Background(), unpushedDiffStartInput("run-3366", unpushedDiffMachine(t), item))
	if err == nil {
		t.Fatal("expected the environmental local-ci fault to surface as a run error")
	}
	if res.Phase != journal.PhaseFailed {
		t.Fatalf("phase = %q, want failed", res.Phase)
	}
	if localCI.calls == 0 {
		t.Fatal("local-ci never ran — the fixture did not exercise the post-validation fault")
	}

	patch, meta, ok := readUnpushedDiffArtifacts(t, runsDir, "run-3366")
	if !ok {
		t.Fatal("no unpushed-diff artifacts in the journal — the implement work was discarded again (#3366)")
	}
	if !strings.Contains(string(patch), "impl.txt") || !strings.Contains(string(patch), "implement work") {
		t.Fatalf("patch artifact does not contain the committed change:\n%s", patch)
	}
	if meta.Schema != unpushedDiffSchemaVersion {
		t.Fatalf("metadata schema = %q, want %q", meta.Schema, unpushedDiffSchemaVersion)
	}
	if meta.RunID != "run-3366" || meta.Stage != "implement" || meta.Workflow != "implementation" {
		t.Fatalf("metadata identity = %+v, want run-3366/implement/implementation", meta)
	}
	if len(meta.ItemIDs) != 1 || meta.ItemIDs[0] != "3366" {
		t.Fatalf("metadata itemIds = %v, want [3366] — re-claim discovery needs the item key", meta.ItemIDs)
	}
	if meta.ItemURL != item.URL {
		t.Fatalf("metadata itemUrl = %q, want %q", meta.ItemURL, item.URL)
	}
	if meta.BaseRef != "main" || meta.Branch == "" {
		t.Fatalf("metadata base/branch = %q/%q, want main and a non-empty run branch", meta.BaseRef, meta.Branch)
	}
	if meta.DiffBytes != len(patch) || meta.Diff.Digest == "" {
		t.Fatalf("metadata diff pointer = %+v, want %d bytes and a digest", meta.Diff, len(patch))
	}

	// The diff must be addressable by the digest the metadata names — that is
	// what a later run resolves.
	rd, err := journal.OpenRead(filepath.Join(runsDir, "run-3366"))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	byDigest, err := rd.ArtifactByDigest(meta.Diff.Digest)
	if err != nil {
		t.Fatalf("ArtifactByDigest(%s): %v", meta.Diff.Digest, err)
	}
	if string(byDigest) != string(patch) {
		t.Fatal("metadata diff digest does not resolve to the recorded patch bytes")
	}

	assertUnpushedDiffSidecarIsDeterministic(t, runsDir, "run-3366")
}

// assertUnpushedDiffSidecarIsDeterministic guards the sidecar's bytes against
// run-varying content. An artifact's content digest is conformance-normative on
// the artifact.recorded event naming it (journal.ConformanceView, ARCHITECTURE
// §3.3), so a wall-clock field in the sidecar makes two identical runs journal
// different digests — which is exactly how a `recordedAt` field broke
// test/e2e's TestConformanceWalkingSkeletonLocalRunnerDeterministicJournal.
// Every key must be a deterministic function of the run's inputs; the recording
// time belongs on the journal event, which conformance excludes.
func assertUnpushedDiffSidecarIsDeterministic(t *testing.T, runsDir, runID string) {
	t.Helper()
	rd, err := journal.OpenRead(filepath.Join(runsDir, runID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	deterministic := map[string]bool{
		"schema": true, "runId": true, "workflow": true, "stage": true, "attempt": true,
		"itemIds": true, "itemUrl": true, "branch": true, "baseRef": true,
		"diffBytes": true, "diff": true,
	}
	var checked int
	for i := range events {
		e := events[i]
		if e.Type != journal.EventArtifactRecorded || e.Ref == nil ||
			!strings.HasSuffix(e.Name, "/"+unpushedDiffMetaName) {
			continue
		}
		raw, err := rd.ArtifactBytes(*e.Ref)
		if err != nil {
			t.Fatalf("read %s: %v", e.Name, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("unmarshal %s: %v", e.Name, err)
		}
		for key := range fields {
			if !deterministic[key] {
				t.Errorf("%s carries non-input-derived field %q — the sidecar digest is "+
					"conformance-normative and must not vary between identical runs", e.Name, key)
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no unpushed-diff sidecar to check")
	}
}

// TestNoUnpushedDiffArtifactWithoutCommittedWork: an agentic stage that
// commits nothing records nothing — the artifact appears only when there is
// real work to strand.
func TestNoUnpushedDiffArtifactWithoutCommittedWork(t *testing.T) {
	localCI := &countingDeterministic{}
	r, runsDir := newUnpushedDiffRunner(t, &noCommitSuccessGoober{}, localCI, nil)

	res, err := r.Start(context.Background(), unpushedDiffStartInput("run-nodiff", unpushedDiffMachine(t), nil))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if _, _, ok := readUnpushedDiffArtifacts(t, runsDir, "run-nodiff"); ok {
		t.Fatal("unpushed-diff artifacts recorded for a stage that committed nothing")
	}
}

// TestUnpushedDiffResolvesItemFromClaimLedger: scheduled/fan-out
// implementation runs claim their item mid-run, so StartInput.Item is nil
// (#796) — the discovery sidecar must fall back to the claim ledger
// (Config.ClaimedItems) for its item key, exactly like the blocked/escalation
// paths do.
func TestUnpushedDiffResolvesItemFromClaimLedger(t *testing.T) {
	goober := &committingSuccessGoober{t: t}
	localCI := &envFaultDeterministic{}
	claimed := func(runID string) ([]string, error) {
		if runID != "run-claimed" {
			return nil, fmt.Errorf("unexpected runID %q", runID)
		}
		return []string{"777"}, nil
	}
	r, runsDir := newUnpushedDiffRunner(t, goober, localCI, claimed)

	if _, err := r.Start(context.Background(), unpushedDiffStartInput("run-claimed", unpushedDiffMachine(t), nil)); err == nil {
		t.Fatal("expected the environmental local-ci fault to surface as a run error")
	}

	_, meta, ok := readUnpushedDiffArtifacts(t, runsDir, "run-claimed")
	if !ok {
		t.Fatal("no unpushed-diff artifacts in the journal")
	}
	if len(meta.ItemIDs) != 1 || meta.ItemIDs[0] != "777" {
		t.Fatalf("metadata itemIds = %v, want [777] from the claim ledger", meta.ItemIDs)
	}
}
