package engine

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	wf "github.com/goobers/goobers/internal/workflow"
)

func boolPointer(value bool) *bool {
	return &value
}

// fakeWorkspaces is a WorkspaceProvisioner backed by temp directories. It
// records every request and teardown so tests can assert the fresh/disposable
// per-attempt workspace contract without git.
type fakeWorkspaces struct {
	mu       sync.Mutex
	root     string
	requests []WorkspaceRequest
	removed  []string
	// provisionErrs are consumed FIFO: each Provision call pops and returns
	// one until the script is exhausted, then provisioning succeeds.
	provisionErrs []error
	emptyPath     bool
	// publish, when set, is what a provisioned workspace reports from
	// PublishDelta (keyed by stage) — the fake's stand-in for the worker's
	// bundle-and-Put. Nil publishes nothing, the pre-#3803 self-arm shape.
	publish func(stage string) (WorkspaceDeltaPublication, error)
	// diff, when set, makes provisioned workspaces implement DiffReader
	// (#3882) — the seam the reviewer-diff capture, the two gate
	// short-circuits and the unpushed-diff capture all read. Keyed by stage so
	// a fixture can script a stage that changes the tree and one that does
	// not; nil leaves the workspace WITHOUT the interface at all, which is the
	// pre-#3882 provisioner shape every other test still asserts against.
	//
	// The two "nothing" answers are deliberately distinguishable, because the
	// production code distinguishes them and the distinction is what licenses
	// the empty-diff fast-fail: a stage present in the map with empty bytes
	// OBSERVED an unchanged branch, a stage absent from it reports an error
	// (the workspace could not tell).
	diff map[string][]byte
	// diffCalls counts Diff() calls per stage, so a test can assert the
	// reviewer's workspace was read exactly once per evaluation.
	diffCalls map[string]int
	// diffSequence, when set for a stage, answers Diff per CALL instead of
	// from diff: entry n for the nth read, with the last entry repeating.
	diffSequence map[string][][]byte
	// headErr, when set for a stage, makes Head fail — the "workspace cannot
	// report" arm that must never fast-fail a gate.
	headErr map[string]error
}

func testWorkspaces(t *testing.T) *fakeWorkspaces {
	t.Helper()
	return &fakeWorkspaces{root: t.TempDir()}
}

// provisionableWorkspaceModes is the set of modes the PRODUCTION provisioner
// (workerhost.WorktreeWorkspaces.Provision) has an arm for — the full
// WorkspaceMode enum plus the unset default. The fake refuses anything else
// exactly as the real one does, so an engine test cannot pass by threading a
// mode the worker would reject; workerhost's
// TestProvisionAcceptsEveryDeclaredWorkspaceMode runs the same table through
// the real provisioner, which is what keeps the two in agreement.
var provisionableWorkspaceModes = map[apiv1.WorkspaceMode]bool{
	"": true, apiv1.WorkspaceRepo: true, apiv1.WorkspaceScratch: true, apiv1.WorkspaceRepoReadOnly: true,
}

func (f *fakeWorkspaces) Provision(_ context.Context, req WorkspaceRequest) (Workspace, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.provisionErrs) > 0 {
		err := f.provisionErrs[0]
		f.provisionErrs = f.provisionErrs[1:]
		return nil, err
	}
	if !provisionableWorkspaceModes[req.Mode] {
		return nil, fmt.Errorf("fakeWorkspaces: unknown workspace mode %q for stage %q (workerhost.WorktreeWorkspaces would refuse it too)", req.Mode, req.Stage)
	}
	f.requests = append(f.requests, req)
	if f.emptyPath {
		return f.wrap(&fakeWorkspace{owner: f, stage: req.Stage}), nil
	}
	path, err := os.MkdirTemp(f.root, fmt.Sprintf("%s-%s-*", req.RunID, req.Stage))
	if err != nil {
		return nil, err
	}
	return f.wrap(&fakeWorkspace{owner: f, path: path, stage: req.Stage}), nil
}

// wrap promotes the workspace to a DiffReader only when the fixture scripted
// diffs. Returning the richer type unconditionally would be the easy thing and
// the wrong one: half this file's existing assertions are about a provisioner
// that CANNOT report a diff, and production has such provisioners (scratch
// workspaces, and any worker predating #3882).
func (f *fakeWorkspaces) wrap(ws *fakeWorkspace) Workspace {
	if f.diff == nil {
		return ws
	}
	return &fakeDiffWorkspace{fakeWorkspace: ws}
}

// scriptDiffSequence registers a stage's diff PER READ: the nth Diff call gets
// the nth entry, and the last entry repeats forever after.
//
// The repetition is the point rather than a convenience. "The stage produced
// the same tree again" is the #316 dedup fixture and "the stage produced a
// different one" is its negative, and both are a statement about a SEQUENCE of
// attempts — a single-entry sequence is the former by construction, so a test
// cannot accidentally write a dedup fixture whose second attempt differs.
func (f *fakeWorkspaces) scriptDiffSequence(stage string, diffs [][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.diffSequence == nil {
		f.diffSequence = map[string][][]byte{}
	}
	f.diffSequence[stage] = diffs
	if f.diff == nil {
		f.diff = map[string][]byte{}
	}
	// Presence in f.diff is what promotes the workspace to a DiffReader.
	f.diff[stage] = nil
}

// scriptDiff registers a stage's diff bytes, creating the map on first use.
func (f *fakeWorkspaces) scriptDiff(stage string, diff []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.diff == nil {
		f.diff = map[string][]byte{}
	}
	f.diff[stage] = diff
}

func (f *fakeWorkspaces) diffCallCount(stage string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.diffCalls[stage]
}

// fakeDiffWorkspace is the DiffReader arm of the fake. Head reports a branch
// name and base ref shaped like the worker's; Diff answers from the script.
type fakeDiffWorkspace struct {
	*fakeWorkspace
}

func (w *fakeDiffWorkspace) Head(_ context.Context) (string, string, error) {
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	if err := w.owner.headErr[w.stage]; err != nil {
		return "", "", err
	}
	return "goobers/wf/" + w.stage, "origin/main", nil
}

func (w *fakeDiffWorkspace) Diff(_ context.Context, _ string) ([]byte, error) {
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	if w.owner.diffCalls == nil {
		w.owner.diffCalls = map[string]int{}
	}
	w.owner.diffCalls[w.stage]++
	if seq, ok := w.owner.diffSequence[w.stage]; ok && len(seq) > 0 {
		i := w.owner.diffCalls[w.stage] - 1
		if i >= len(seq) {
			i = len(seq) - 1
		}
		return seq[i], nil
	}
	diff, ok := w.owner.diff[w.stage]
	if !ok {
		return nil, fmt.Errorf("fakeDiffWorkspace: stage %q has no scripted diff", w.stage)
	}
	return diff, nil
}

func (f *fakeWorkspaces) provisioned() []WorkspaceRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]WorkspaceRequest(nil), f.requests...)
}

func (f *fakeWorkspaces) removedPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.removed...)
}

type fakeWorkspace struct {
	owner *fakeWorkspaces
	path  string
	stage string
}

func (w *fakeWorkspace) Path() string { return w.path }

// PublishDelta implements DeltaPublisher through the owner's publish hook.
func (w *fakeWorkspace) PublishDelta(context.Context) (WorkspaceDeltaPublication, error) {
	w.owner.mu.Lock()
	publish := w.owner.publish
	w.owner.mu.Unlock()
	if publish == nil {
		return WorkspaceDeltaPublication{}, nil
	}
	return publish(w.stage)
}

func (w *fakeWorkspace) Remove(context.Context) error {
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	w.owner.removed = append(w.owner.removed, w.path)
	return os.RemoveAll(w.path)
}

// linearSpec is a single-stage, implement-only workflow — the shape the engine's
// happy-path run tests walk.
func linearSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement"},
		},
	}
}

// gatedSpec is an implement→review workflow whose reviewer gate can pass, abort,
// or loop back for changes — the shape the engine's branching tests walk.
func gatedSpec() apiv1.WorkflowSpec {
	return apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement", Next: "review"},
		},
		Gates: []apiv1.Gate{
			{
				Name:      "review",
				Evaluator: apiv1.EvaluatorAgentic,
				Agentic:   &apiv1.AgenticGate{Goober: "reviewer"},
				Branches: map[string]string{
					"pass":          wf.TerminalComplete,
					"fail":          wf.TargetAbort,
					"needs-changes": "implement",
				},
			},
		},
	}
}
