package engine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/sdk/testsuite"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// envelopeDigest is a syntactically valid sha256 digest for fixture artifacts.
const envelopeDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// capturingDeterministic records every envelope it is dispatched with and
// returns a canned result — usable behind both runners' invoke.Deterministic
// seam, which is what makes the cross-runner envelope parity test possible.
type capturingDeterministic struct {
	mu     sync.Mutex
	envs   []apiv1.InvocationEnvelope
	result apiv1.ResultEnvelope
}

func (c *capturingDeterministic) Run(_ context.Context, env apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.envs = append(c.envs, env)
	if c.result.Status == "" {
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}
	return c.result, nil
}

func (c *capturingDeterministic) captured() []apiv1.InvocationEnvelope {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]apiv1.InvocationEnvelope(nil), c.envs...)
}

// TestBuildInvocationCompleteEnvelope is #621's headline acceptance: the
// engine-built envelope carries workspace, limits, capabilities, and the rest
// of the closed invocation schema's fields, and the JSON it serializes to
// validates against that schema (workspace required, no omitted fields).
func TestBuildInvocationCompleteEnvelope(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement the fix",
			Inputs:         map[string]string{"style": "tidy"},
			Capabilities:   []string{"github:issues:write", "repo:push"},
			TimeoutSeconds: 45,
			Limits:         &apiv1.Limits{MaxTokens: 2000, MaxCostUSD: 3.5},
		}},
	}
	in := runInput("complete", spec)
	in.TriggerRef = "item#42"
	in.BranchNamespace = "goobers/"
	in.Item = &apiv1.BacklogItem{ID: "42", Provider: apiv1.ProviderGitHub, Title: "Fix bug"}

	workspaces := testWorkspaces(t)
	var captured apiv1.InvocationEnvelope
	inv := &fakeInvoker{invoke: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
		captured = env
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Goober: inv, Workspaces: workspaces})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}

	if captured.TaskID != in.RunID+":implement" {
		t.Errorf("taskId = %q, want %q", captured.TaskID, in.RunID+":implement")
	}
	if captured.Workspace == "" {
		t.Fatal("envelope workspace is empty — the closed invocation schema requires it")
	}
	provisioned := workspaces.provisioned()
	if len(provisioned) != 1 || provisioned[0].Stage != "implement" || provisioned[0].Mode != apiv1.WorkspaceRepo {
		t.Fatalf("workspace requests = %+v, want one repo-mode request for implement", provisioned)
	}
	if !strings.HasPrefix(filepath.Base(captured.Workspace), in.RunID) {
		t.Errorf("workspace %q was not the provisioned attempt directory", captured.Workspace)
	}
	if want := []string{"github:issues:write", "repo:push"}; !reflect.DeepEqual(captured.Capabilities, want) {
		t.Errorf("capabilities = %v, want %v — grants must survive engine dispatch, not be dropped", captured.Capabilities, want)
	}
	if captured.Limits.MaxDurationSeconds != 45 || captured.Limits.MaxTokens != 2000 || captured.Limits.MaxCostUSD != 3.5 {
		t.Errorf("limits = %+v, want the task's declared limits", captured.Limits)
	}
	if captured.TriggerRef != "item#42" {
		t.Errorf("triggerRef = %q, want item#42", captured.TriggerRef)
	}
	if captured.BranchNamespace != "goobers/" {
		t.Errorf("branchNamespace = %q, want goobers/", captured.BranchNamespace)
	}
	if captured.Item == nil || captured.Item.ID != "42" {
		t.Errorf("item = %+v, want the pinned backlog item", captured.Item)
	}
	if captured.Inputs["style"] != "tidy" {
		t.Errorf("inputs = %+v, want the task's static inputs", captured.Inputs)
	}

	validator, err := validate.New()
	if err != nil {
		t.Fatalf("build schema validator: %v", err)
	}
	raw, err := json.Marshal(captured)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := validator.ValidateJSON("invocation.schema.json", raw); err != nil {
		t.Fatalf("engine envelope does not validate against the closed invocation schema: %v", err)
	}
}

// TestEnvelopeContextPointersThreadUpstreamArtifacts: a downstream stage
// receives upstream artifacts as read-only ContextPointers (§2.4), named
// exactly as the local runner names them.
func TestEnvelopeContextPointersThreadUpstreamArtifacts(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "gather",
		Tasks: []apiv1.Task{
			{Name: "gather", Type: apiv1.TaskDeterministic, Goal: "gather evidence",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
				Next: "implement"},
			{Name: "implement", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "implement"},
		},
	}
	det := &capturingDeterministic{result: apiv1.ResultEnvelope{
		Status: apiv1.ResultSuccess,
		Artifacts: []apiv1.ArtifactPointer{{
			Path: "stages/gather/1/evidence.json", Digest: envelopeDigest, Size: 12, MediaType: "application/json",
		}},
	}}
	var captured apiv1.InvocationEnvelope
	inv := &fakeInvoker{invoke: func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
		captured = env
		return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
	}}

	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Goober: inv, Det: det, Workspaces: testWorkspaces(t)})
	env.ExecuteWorkflow(Run, runInput("pointers", spec))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}

	if len(captured.ContextPointers) != 1 {
		t.Fatalf("context pointers = %+v, want exactly the upstream artifact", captured.ContextPointers)
	}
	ptr := captured.ContextPointers[0]
	if ptr.Name != "gather.artifact[0]" {
		t.Errorf("pointer name = %q, want gather.artifact[0] (local-runner naming)", ptr.Name)
	}
	if ptr.Artifact == nil || ptr.Artifact.Path != "stages/gather/1/evidence.json" || ptr.Artifact.Digest != envelopeDigest {
		t.Errorf("pointer artifact = %+v, want the upstream stage's recorded artifact", ptr.Artifact)
	}
}

// TestEnvelopeInputsFromOverlaysUpstreamOutputs: the #132 output->input
// handoff threads a declared upstream output into the next task's inputs, and
// a missing declared key fails the stage closed.
func TestEnvelopeInputsFromOverlaysUpstreamOutputs(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "select",
		Tasks: []apiv1.Task{
			{Name: "select", Type: apiv1.TaskDeterministic, Goal: "select a PR",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
				Next: "remediate"},
			{Name: "remediate", Type: apiv1.TaskDeterministic, Goal: "remediate the PR",
				Run:        &apiv1.DeterministicRun{Command: []string{"true"}},
				Inputs:     map[string]string{"mode": "safe"},
				InputsFrom: map[string]string{"prNumber": "selectedPr"}},
		},
	}

	t.Run("threads the declared output", func(t *testing.T) {
		det := &capturingDeterministic{result: apiv1.ResultEnvelope{
			Status:  apiv1.ResultSuccess,
			Outputs: map[string]interface{}{"selectedPr": "1287"},
		}}
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		env.RegisterActivity(&Activities{Det: det, Workspaces: testWorkspaces(t)})
		env.ExecuteWorkflow(Run, runInput("inputsfrom", spec))
		if err := env.GetWorkflowError(); err != nil {
			t.Fatalf("workflow error: %v", err)
		}
		envs := det.captured()
		if len(envs) != 2 {
			t.Fatalf("dispatches = %d, want 2", len(envs))
		}
		got := envs[1].Inputs
		if got["prNumber"] != "1287" || got["mode"] != "safe" {
			t.Fatalf("remediate inputs = %+v, want the static input plus the threaded upstream output", got)
		}
	})

	t.Run("missing declared output fails closed", func(t *testing.T) {
		det := &capturingDeterministic{} // success, but no outputs at all
		var ts testsuite.WorkflowTestSuite
		env := ts.NewTestWorkflowEnvironment()
		env.RegisterActivity(&Activities{Det: det, Workspaces: testWorkspaces(t)})
		env.ExecuteWorkflow(Run, runInput("inputsfrom-missing", spec))
		err := env.GetWorkflowError()
		if err == nil || !strings.Contains(err.Error(), `upstream output "selectedPr" not found`) {
			t.Fatalf("workflow error = %v, want the inputsFrom fail-closed error", err)
		}
		if got := len(det.captured()); got != 1 {
			t.Fatalf("dispatches = %d, want 1 — the stage must not dispatch with a partial contract", got)
		}
	})
}

// TestAgenticGateEnvelopeCarriesReviewerGrantsAndPointers: an agentic gate's
// envelope carries the reviewer goober's pinned capability grants (#294
// parity) and the upstream evidence pointers; goal naming matches the local
// runner's "gate: <name>".
func TestAgenticGateEnvelopeCarriesReviewerGrantsAndPointers(t *testing.T) {
	in := runInput("gated-caps", gatedSpec())
	in.GateGooberCapabilities = map[string][]string{"reviewer": {"agent:model"}}

	inv := successInvoker()
	var captured apiv1.InvocationEnvelope
	inv.invoke = func(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
		return apiv1.ResultEnvelope{
			Status: apiv1.ResultSuccess,
			Artifacts: []apiv1.ArtifactPointer{{
				Path: "stages/implement/1/diff.patch", Digest: envelopeDigest, Size: 7, MediaType: "text/x-diff",
			}},
		}, nil
	}
	inv.review = func(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
		captured = env
		return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
	}

	workspaces := testWorkspaces(t)
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Goober: inv, Workspaces: workspaces})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}

	if want := []string{"agent:model"}; !reflect.DeepEqual(captured.Capabilities, want) {
		t.Errorf("gate capabilities = %v, want %v (the reviewer goober's pinned grants)", captured.Capabilities, want)
	}
	if captured.Goal != "gate: review" {
		t.Errorf("gate goal = %q, want %q (local-runner naming)", captured.Goal, "gate: review")
	}
	if captured.Workspace == "" {
		t.Error("agentic gate envelope has no workspace — the reviewer runs a real goober subprocess")
	}
	if len(captured.ContextPointers) != 1 || captured.ContextPointers[0].Name != "implement.artifact[0]" {
		t.Errorf("gate context pointers = %+v, want the subject stage's artifact", captured.ContextPointers)
	}
	if got := len(workspaces.provisioned()); got != 2 {
		t.Errorf("workspaces provisioned = %d, want 2 (implement + the reviewer gate)", got)
	}
}

// TestAutomatedGateGetsNoWorkspace mirrors the local runner's #112 contract:
// an automated gate's checks are pure functions over env.Inputs, so no
// workspace is provisioned for it.
func TestAutomatedGateGetsNoWorkspace(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "implement",
		Tasks: []apiv1.Task{{
			Name: "implement", Type: apiv1.TaskDeterministic, Goal: "produce a diff",
			Run:  &apiv1.DeterministicRun{Command: []string{"true"}},
			Next: "check",
		}},
		Gates: []apiv1.Gate{{
			Name:      "check",
			Evaluator: apiv1.EvaluatorAutomated,
			Automated: &apiv1.AutomatedGate{Check: "status-equals"},
			Branches:  map[string]string{"pass": wf.TerminalComplete, "fail": wf.TargetAbort},
		}},
	}
	workspaces := testWorkspaces(t)
	var gateEnv apiv1.InvocationEnvelope
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{
		Det: &capturingDeterministic{},
		Auto: automatedFunc(func(_ context.Context, _ apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
			gateEnv = env
			return "pass", nil
		}),
		Workspaces: workspaces,
	})
	env.ExecuteWorkflow(Run, runInput("auto-gate", spec))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if gateEnv.Workspace != "" {
		t.Errorf("automated gate workspace = %q, want empty (#112: no worktree for pure checks)", gateEnv.Workspace)
	}
	if len(gateEnv.Capabilities) != 0 {
		t.Errorf("automated gate capabilities = %v, want none", gateEnv.Capabilities)
	}
	provisioned := workspaces.provisioned()
	if len(provisioned) != 1 || provisioned[0].Stage != "implement" {
		t.Errorf("workspace requests = %+v, want only the task's", provisioned)
	}
}

// automatedFunc adapts a function to invoke.Automated for tests.
type automatedFunc func(ctx context.Context, gate apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error)

func (f automatedFunc) Evaluate(ctx context.Context, gate apiv1.AutomatedGate, env apiv1.InvocationEnvelope) (string, error) {
	return f(ctx, gate, env)
}

// TestWorkspaceFailuresFailClosed: a stage whose envelope cannot be fully
// constructed errors — it never dispatches a partial envelope (#621).
func TestWorkspaceFailuresFailClosed(t *testing.T) {
	cases := []struct {
		name       string
		workspaces WorkspaceProvisioner
		wantErr    string
	}{
		{"no provisioner wired", nil, "no provisioner is wired"},
		{"empty path", &fakeWorkspaces{emptyPath: true}, "returned no path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invoked := false
			inv := &fakeInvoker{invoke: func(_ context.Context, _ apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
				invoked = true
				return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
			}}
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			env.RegisterActivity(&Activities{Goober: inv, Workspaces: tc.workspaces})
			env.ExecuteWorkflow(Run, runInput("fail-closed", linearSpec()))
			err := env.GetWorkflowError()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("workflow error = %v, want %q", err, tc.wantErr)
			}
			if invoked {
				t.Fatal("the goober seam was invoked despite the workspace failure — partial envelope dispatched")
			}
		})
	}
}

// TestWorkspaceRemovedPerAttempt: every provisioned working copy is disposed
// after its attempt (fresh/disposable stage contract, §5).
func TestWorkspaceRemovedPerAttempt(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "one",
		Tasks: []apiv1.Task{
			{Name: "one", Type: apiv1.TaskDeterministic, Goal: "first",
				Run:  &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
				Next: "two"},
			{Name: "two", Type: apiv1.TaskAgentic, Goober: "coder", Goal: "second"},
		},
	}
	workspaces := testWorkspaces(t)
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{
		Goober: successInvoker(), Det: &capturingDeterministic{}, Workspaces: workspaces,
	})
	env.ExecuteWorkflow(Run, runInput("dispose", spec))
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	provisioned := workspaces.provisioned()
	if len(provisioned) != 2 {
		t.Fatalf("workspace requests = %+v, want 2", provisioned)
	}
	if provisioned[0].Mode != apiv1.WorkspaceScratch {
		t.Errorf("first stage mode = %q, want scratch (from the task's run block)", provisioned[0].Mode)
	}
	if provisioned[1].Mode != apiv1.WorkspaceRepo {
		t.Errorf("agentic stage mode = %q, want repo", provisioned[1].Mode)
	}
	if removed := workspaces.removedPaths(); len(removed) != 2 {
		t.Errorf("removed workspaces = %d, want 2 — every attempt's copy is disposable", len(removed))
	}
}

// TestEngineEnvelopeMatchesLocalRunner is #621's cross-runner parity
// acceptance: for the same compiled definition and stage, the envelope the
// engine builds equals the one the local runner builds on every
// conformance-relevant field — Workspace being the one runner-specific field,
// cleared before comparison.
func TestEngineEnvelopeMatchesLocalRunner(t *testing.T) {
	spec := apiv1.WorkflowSpec{
		Gaggle:   "web",
		Triggers: []apiv1.Trigger{{Type: apiv1.TriggerBacklogItem}},
		Start:    "audit",
		Tasks: []apiv1.Task{{
			Name: "audit", Type: apiv1.TaskDeterministic, Goal: "audit the backlog",
			Run:            &apiv1.DeterministicRun{Command: []string{"true"}, Workspace: apiv1.WorkspaceScratch},
			Inputs:         map[string]string{"kind": "audit"},
			Capabilities:   []string{"github:issues:write"},
			TimeoutSeconds: 30,
			Limits:         &apiv1.Limits{MaxTokens: 500},
		}},
	}
	const runID = "run-envelope-parity"
	item := &apiv1.BacklogItem{ID: "7", Provider: apiv1.ProviderGitHub, Title: "Audit"}
	repoRef := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web"}
	machine, err := wf.Compile(wf.Definition{Name: "parity", Version: 1, Spec: spec}, wf.WithPreviewFeatures(true))
	if err != nil {
		t.Fatalf("compile parity machine: %v", err)
	}

	// Local runner side: a scratch-workspace deterministic stage needs no git
	// fixture, so the real runner walks the machine hermetically.
	local := &capturingDeterministic{}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	r, err := runner.New(runner.Config{
		NewDeterministic: func(runner.ArtifactRecorder, runner.SecretRegistrar) (invoke.Deterministic, error) {
			return local, nil
		},
		Worktrees:        wtMgr,
		RunsDir:          filepath.Join(instanceRoot, "runs"),
		ScratchDir:       filepath.Join(instanceRoot, "scratch"),
		BranchNamespaces: map[string]string{"web": "goobers"},
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	res, err := r.Start(context.Background(), runner.StartInput{
		RunID:   runID,
		Machine: machine,
		Gaggle:  "web",
		Trigger: journal.Trigger{Kind: journal.TriggerItem, Ref: "item#7"},
		RepoRef: repoRef,
		Item:    item,
	})
	if err != nil {
		t.Fatalf("local runner Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("local phase = %q, want completed", res.Phase)
	}

	// Engine side: the same definition, pinned into a RunInput carrying the
	// same trigger/namespace policy the local runner was configured with.
	engineSide := &capturingDeterministic{}
	in := RunInput{
		RunID:                  runID,
		Gaggle:                 "web",
		WorkflowName:           "parity",
		Version:                1,
		PreviewFeaturesEnabled: boolPointer(true),
		Spec:                   spec,
		RepoRef:                repoRef,
		Item:                   item,
		TriggerRef:             "item#7",
		BranchNamespace:        providers.NormalizeBranchNamespace("goobers"),
	}
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivity(&Activities{Det: engineSide, Workspaces: testWorkspaces(t)})
	env.ExecuteWorkflow(Run, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("engine workflow error: %v", err)
	}

	localEnvs, engineEnvs := local.captured(), engineSide.captured()
	if len(localEnvs) != 1 || len(engineEnvs) != 1 {
		t.Fatalf("dispatch counts local=%d engine=%d, want 1 each", len(localEnvs), len(engineEnvs))
	}
	le, ee := localEnvs[0], engineEnvs[0]
	if le.Workspace == "" || ee.Workspace == "" {
		t.Fatalf("workspaces local=%q engine=%q, want both populated", le.Workspace, ee.Workspace)
	}
	// Workspace paths are runner-specific (each provisions its own disposable
	// copy) and excluded from the conformance surface; everything else must
	// match exactly.
	le.Workspace, ee.Workspace = "", ""
	if !reflect.DeepEqual(le, ee) {
		t.Errorf("envelopes diverge across runners:\nlocal:  %+v\nengine: %+v", le, ee)
	}
}
