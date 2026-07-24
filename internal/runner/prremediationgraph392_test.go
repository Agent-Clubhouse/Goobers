package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
	"github.com/goobers/goobers/providers"
)

// remediationGoober is the fake standing in for BOTH agentic roles in
// pr-remediation: the `implement` stage (Invoke) and the `review` gate's
// reviewer (Review). implement commits a real change, because since #415 an
// empty diff fast-fails the reviewer gate before it is ever invoked — a stub
// that committed nothing would make this test pass through a path the live
// workflow never takes.
type remediationGoober struct {
	t                 *testing.T
	mu                sync.Mutex
	verdicts          []apiv1.VerdictDecision
	invoked           int
	reviewed          int
	sawSiblingContext bool
	sawReviewThreads  bool
	// visitMu/visited are the SHARED dispatch log the deterministic stub also
	// appends to — an agentic stage goes through a different executor, so
	// recording only deterministic dispatches would silently omit `implement`,
	// the one stage this whole issue is about.
	visitMu *sync.Mutex
	visited *[]string
}

func (g *remediationGoober) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.t.Helper()
	_, stage, _ := strings.Cut(env.TaskID, ":")
	g.visitMu.Lock()
	*g.visited = append(*g.visited, stage)
	g.visitMu.Unlock()
	g.mu.Lock()
	n := g.invoked
	g.invoked++
	for _, pointer := range env.ContextPointers {
		if pointer.Name == "gather-sibling-context.artifact[0]" && pointer.Artifact != nil {
			g.sawSiblingContext = true
		}
		if pointer.Name == "gather-review-threads.artifact[0]" && pointer.Artifact != nil {
			g.sawReviewThreads = true
		}
	}
	g.mu.Unlock()
	// A distinct change per pass, so a repass produces a genuinely different
	// diff (#316's same-diff short-circuit would otherwise escalate).
	name := filepath.Join(env.Workspace, "remediation.txt")
	body := strings.Repeat("addressed a finding\n", n+1)
	if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	runGit(g.t, env.Workspace, "add", "-A")
	runGit(g.t, env.Workspace, "commit", "-m", "address merge-review findings")
	return apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Summary: "remediated",
		Outputs: map[string]interface{}{"findingResponses": "[]"},
	}, nil
}

func (g *remediationGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	decision := apiv1.VerdictPass
	if g.reviewed < len(g.verdicts) {
		decision = g.verdicts[g.reviewed]
	}
	g.reviewed++
	return apiv1.Verdict{Decision: decision, Rationale: "test verdict"}, nil
}

// loadShippedPRRemediation compiles the REAL shipped pr-remediation definition,
// so this test walks the graph the dogfood instance actually runs rather than a
// synthetic re-statement of it that could drift away from the YAML silently.
func loadShippedPRRemediation(t *testing.T) *workflow.Machine {
	t.Helper()
	root := filepath.Join("..", "..", "selfhost", "gaggles", "goobers")

	raw, err := os.ReadFile(filepath.Join(root, "workflows", "pr-remediation.yaml"))
	if err != nil {
		t.Fatalf("read pr-remediation.yaml: %v", err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal pr-remediation.yaml: %v", err)
	}

	goobers := map[string]apiv1.GooberSpec{}
	for _, name := range []string{"implementer", "reviewer"} {
		var g apiv1.Goober
		graw, err := os.ReadFile(filepath.Join(root, "goobers", name, "goober.yaml"))
		if err != nil {
			t.Fatalf("read %s goober: %v", name, err)
		}
		if err := yaml.Unmarshal(graw, &g); err != nil {
			t.Fatalf("unmarshal %s goober: %v", name, err)
		}
		goobers[g.Name] = g.Spec
	}

	m, err := workflow.Compile(
		workflow.Definition{Name: w.Name, Version: 1, Spec: w.Spec},
		workflow.WithGoobers(goobers),
		workflow.WithKnownChecks([]string{"output-equals", "status-equals"}),
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile shipped pr-remediation: %v", err)
	}
	return m
}

// visitRecordingDeterministic records the order stages were dispatched in and
// serves each one canned outputs, standing in for the provider-chain CLIs
// (gather-pr-context, rebase-pr, remediation-checkpoint,
// validate-finding-responses, push-remediated, respond-to-findings) and for
// `make ci`.
type visitRecordingDeterministic struct {
	t       *testing.T
	rec     ArtifactRecorder
	byTask  map[string]stubTaskResult
	mu      *sync.Mutex
	visited *[]string
}

func (v *visitRecordingDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, dr apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	_, stage, _ := strings.Cut(env.TaskID, ":")
	v.mu.Lock()
	*v.visited = append(*v.visited, stage)
	v.mu.Unlock()
	return (&stubDeterministic{rec: v.rec, byTask: v.byTask}).Run(ctx, env, dr)
}

type remediationWalkOptions struct {
	maxRepasses      int
	validationStatus apiv1.ResultStatus
}

// walkShippedPRRemediation drives one run of the real graph and returns the
// terminal result plus the stage visit order. rebindTo is the branch
// gather-pr-context reports (the selected PR's head).
func walkShippedPRRemediation(t *testing.T, runID string, goober *remediationGoober, options ...remediationWalkOptions) (Result, []string, string) {
	t.Helper()
	opts := remediationWalkOptions{validationStatus: apiv1.ResultSuccess}
	if len(options) > 0 {
		opts = options[0]
	}
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	fixtureRepo := newRebindFixtureRepo(t)

	var mu sync.Mutex
	visited := []string{}
	goober.visitMu, goober.visited = &mu, &visited

	// The selection outcome that routes down the agentic path: a substantive
	// finding present, so rebase-pr reports needsAgent=true.
	byTask := map[string]stubTaskResult{
		runID + ":update-behind-pr": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{
			"selectedNumber": "77", "needsFullRemediation": "true",
		}},
		runID + ":gather-pr-context": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{
			WorkspaceBranchOutput:    rebindBranch,
			"selectedNumber":         "77",
			"head":                   rebindBranch,
			"base":                   "main",
			"isBehindBase":           "true",
			"hasSubstantiveFindings": "true",
			"hasFailingCI":           "false",
		}},
		runID + ":rebase-pr": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{
			"selectedNumber": "77", "head": rebindBranch, "needsAgent": "true",
		}},
		runID + ":remediation-checkpoint": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{
			"continueRemediation": "true", "selectedNumber": "77",
			"head": rebindBranch, "headSha": "deadbeef",
		}},
		runID + ":gather-sibling-context": {
			status:       apiv1.ResultSuccess,
			outputs:      map[string]interface{}{"selectedNumber": "77"},
			artifactName: "sibling-context.json", artifactData: []byte(`{"siblings":[]}`),
			artifactMediaType: "application/json",
		},
		runID + ":gather-review-threads": {
			status:            apiv1.ResultSuccess,
			artifactName:      "remediation-brief.json",
			artifactData:      []byte(`{"schema":"goobers.dev/remediation-brief/v2","selectedNumber":"77","head":"goobers/implementation/pr-head","base":"main","workspaceBranch":"goobers/implementation/pr-head","isBehindBase":true,"hasSubstantiveFindings":"true","hasFailingCI":"false","gatherPrContext":{"headSha":"head","baseSha":"base","verdict":null,"comments":[]},"gatherReviewThreads":{"reviews":[],"inlineComments":[]}}`),
			artifactMediaType: "application/json",
		},
		runID + ":validate-finding-responses": {status: opts.validationStatus},
		runID + ":local-ci":                   {status: apiv1.ResultSuccess},
		runID + ":push-remediated": {
			status: apiv1.ResultSuccess, outputs: map[string]interface{}{"published": "true"},
		},
		runID + ":respond-to-findings": {
			status: apiv1.ResultSuccess, outputs: map[string]interface{}{"posted": true},
		},
		runID + ":park-escalated":                 {status: apiv1.ResultSuccess},
		runID + ":park-invalid-finding-responses": {status: apiv1.ResultSuccess},
	}

	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &visitRecordingDeterministic{t: t, rec: rec, byTask: byTask, mu: &mu, visited: &visited}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return goober, nil
		},
		Automated:              gate.NewAutomatedEvaluator(),
		MaxRepasses:            opts.maxRepasses,
		GateGooberCapabilities: map[string][]string{"reviewer": {"agent:model"}},
		Worktrees:              wtMgr,
		ScratchDir:             filepath.Join(instanceRoot, "scratch"),
		RunsDir:                filepath.Join(instanceRoot, "runs"),
		RepoCloneURL:           func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: loadShippedPRRemediation(t), Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	return res, append([]string(nil), visited...), fixtureRepo
}

// TestShippedPRRemediationWalksTheFullAgenticChain is the integration test
// #392 exists to make possible, and the one whose ABSENCE is the pattern this
// repo has been bitten by before (merge-review's L1: 100%-broken wiring passed
// every unit test because nothing walked the graph).
//
// It compiles the real shipped YAML and walks it through the real runner with
// real git worktrees. A PR with a substantive finding must travel
// gather-pr-context → rebase-pr → [needs agent] → remediation-checkpoint →
// [continue] → gather-sibling-context → gather-review-threads → implement → validate-finding-responses
// → [response validation pass] → [review pass] → local-ci → [ci pass] →
// push-remediated → respond-to-findings, and complete. Before #392 this run
// dead-ended at remediation-checkpoint.
func TestShippedPRRemediationWalksTheFullAgenticChain(t *testing.T) {
	goober := &remediationGoober{t: t}
	res, visited, _ := walkShippedPRRemediation(t, "prr-full", goober)

	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want %q (visited: %v)", res.Phase, journal.PhaseCompleted, visited)
	}
	want := []string{
		"update-behind-pr",
		"gather-pr-context",
		"rebase-pr",
		"remediation-checkpoint",
		"gather-sibling-context",
		"gather-review-threads",
		"implement",
		"validate-finding-responses",
		"local-ci",
		"push-remediated",
		"respond-to-findings",
	}
	if strings.Join(visited, ",") != strings.Join(want, ",") {
		t.Errorf("stage order = %v, want %v", visited, want)
	}
	if goober.reviewed != 1 {
		t.Errorf("reviewer invoked %d times, want 1 — the agentic review gate must actually run", goober.reviewed)
	}
	if !goober.sawSiblingContext {
		t.Error("implementer context is missing gather-sibling-context.artifact[0]")
	}
	if !goober.sawReviewThreads {
		t.Error("implementer context is missing gather-review-threads.artifact[0]")
	}
}

func TestShippedPRRemediationAPIUpdateProvisionsNoWorktree(t *testing.T) {
	const runID = "prr-api-update"
	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	var mu sync.Mutex
	visited := []string{}
	cloneURLCalls := 0
	byTask := map[string]stubTaskResult{
		runID + ":update-behind-pr": {
			status: apiv1.ResultSuccess,
			outputs: map[string]interface{}{
				"selectedNumber": "77", "needsFullRemediation": "false",
			},
		},
	}
	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &visitRecordingDeterministic{t: t, rec: rec, byTask: byTask, mu: &mu, visited: &visited}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return &remediationGoober{t: t}, nil
		},
		Automated:  gate.NewAutomatedEvaluator(),
		Worktrees:  wtMgr,
		ScratchDir: filepath.Join(instanceRoot, "scratch"),
		RunsDir:    filepath.Join(instanceRoot, "runs"),
		RepoCloneURL: func(apiv1.RepoRef) (string, error) {
			cloneURLCalls++
			return "", fmt.Errorf("repo workspace must not be requested on API update path")
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: loadShippedPRRemediation(t), Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want completed", res.Phase)
	}
	if strings.Join(visited, ",") != "update-behind-pr" {
		t.Fatalf("dispatched stages = %v, want only update-behind-pr", visited)
	}
	if cloneURLCalls != 0 {
		t.Fatalf("RepoCloneURL called %d times, want 0 worktree provisions", cloneURLCalls)
	}
	reader, err := journal.OpenRead(filepath.Join(instanceRoot, "runs", runID))
	if err != nil {
		t.Fatalf("open run journal: %v", err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("read run journal: %v", err)
	}
	var started []string
	for _, event := range events {
		if event.Type == journal.EventStageStarted {
			started = append(started, event.Stage)
		}
	}
	if strings.Join(started, ",") != "update-behind-pr" {
		t.Fatalf("journaled stages = %v, want no rebase stage", started)
	}
}

// TestShippedPRRemediationRepassesOnNeedsChanges proves the loop the chain
// depends on: a needs-changes verdict returns to implement rather than
// escalating or dead-ending, and the second pass still lands on the PR's
// branch (the rebinding is sticky across the loop-back, not consumed by the
// first traversal).
func TestShippedPRRemediationRepassesOnNeedsChanges(t *testing.T) {
	goober := &remediationGoober{t: t, verdicts: []apiv1.VerdictDecision{apiv1.VerdictNeedsChanges, apiv1.VerdictPass}}
	res, visited, _ := walkShippedPRRemediation(t, "prr-repass", goober)

	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want %q (visited: %v)", res.Phase, journal.PhaseCompleted, visited)
	}
	implements := 0
	for _, s := range visited {
		if s == "implement" {
			implements++
		}
	}
	if implements != 2 {
		t.Errorf("implement dispatched %d times, want 2 (visited: %v)", implements, visited)
	}
	if visited[len(visited)-1] != "respond-to-findings" {
		t.Errorf("last stage = %q, want respond-to-findings after the remediated branch was published", visited[len(visited)-1])
	}
}

// TestShippedPRRemediationEscalatesOnReviewerFail covers design doc §4 D2: a
// terminal `fail` verdict must park the PR for a human rather than spend
// further remediation budget, and the run must end ESCALATED (every escalation
// surface keys on the phase, not on an abort).
func TestShippedPRRemediationEscalatesOnReviewerFail(t *testing.T) {
	goober := &remediationGoober{t: t, verdicts: []apiv1.VerdictDecision{apiv1.VerdictFail}}
	res, visited, _ := walkShippedPRRemediation(t, "prr-fail", goober)

	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want %q (visited: %v)", res.Phase, journal.PhaseEscalated, visited)
	}
	if visited[len(visited)-1] != "park-escalated" {
		t.Errorf("last stage = %q, want park-escalated", visited[len(visited)-1])
	}
	for _, s := range visited {
		if s == "push-remediated" {
			t.Error("a reviewer-rejected remediation must never be published to the PR")
		}
	}
}

func TestShippedPRRemediationParksOnFindingResponseValidationExhaustion(t *testing.T) {
	goober := &remediationGoober{t: t}
	res, visited, _ := walkShippedPRRemediation(t, "prr-invalid-responses", goober, remediationWalkOptions{
		maxRepasses:      1,
		validationStatus: apiv1.ResultFailure,
	})

	if res.Phase != journal.PhaseEscalated {
		t.Fatalf("phase = %q, want %q (visited: %v)", res.Phase, journal.PhaseEscalated, visited)
	}
	if visited[len(visited)-1] != "park-invalid-finding-responses" {
		t.Errorf("last stage = %q, want park-invalid-finding-responses", visited[len(visited)-1])
	}
	if goober.reviewed != 0 {
		t.Errorf("reviewer invoked %d times, want 0 for invalid finding responses", goober.reviewed)
	}
	var implements, validations int
	for _, stage := range visited {
		switch stage {
		case "implement":
			implements++
		case "validate-finding-responses":
			validations++
		}
		if stage == "local-ci" || stage == "push-remediated" {
			t.Errorf("invalid finding responses reached %s before parking", stage)
		}
	}
	if implements != 2 || validations != 2 {
		t.Errorf("implement/validation visits = %d/%d, want 2/2 before parking (visited: %v)", implements, validations, visited)
	}
}

// TestShippedImplementationIsUnaffectedByTheRebindingSeam is the regression
// guard for the CORE loop. #392 threads a new branch-rebinding parameter
// through runTask/dispatchTask/evaluateGate/buildEnvelope/createStageWorkspace
// — the code path EVERY workflow's every stage takes. `implementation` is the
// workflow this project actually runs on itself daily, and it emits no
// workspaceBranch anywhere, so it must be affected in exactly no way.
//
// Asserting the traversal alone would be too weak: the failure mode that would
// matter is a stage silently getting provisioned on the wrong branch while the
// graph still walks correctly. So this also checks the branch every stage's
// worktree actually landed on.
func TestShippedImplementationIsUnaffectedByTheRebindingSeam(t *testing.T) {
	const runID = "impl-unaffected"
	root := filepath.Join("..", "..", "selfhost", "gaggles", "goobers")

	raw, err := os.ReadFile(filepath.Join(root, "workflows", "implementation.yaml"))
	if err != nil {
		t.Fatalf("read implementation.yaml: %v", err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal implementation.yaml: %v", err)
	}
	goobers := map[string]apiv1.GooberSpec{}
	for _, name := range []string{"implementer", "reviewer"} {
		var g apiv1.Goober
		graw, err := os.ReadFile(filepath.Join(root, "goobers", name, "goober.yaml"))
		if err != nil {
			t.Fatalf("read %s goober: %v", name, err)
		}
		if err := yaml.Unmarshal(graw, &g); err != nil {
			t.Fatalf("unmarshal %s goober: %v", name, err)
		}
		goobers[g.Name] = g.Spec
	}
	machine, err := workflow.Compile(
		workflow.Definition{Name: w.Name, Version: 1, Spec: w.Spec},
		workflow.WithGoobers(goobers),
		// #947: open-pr-gate uses output-equals(opened) to abort on a
		// mid-flight-closed issue.
		workflow.WithKnownChecks([]string{"status-equals", "ci-status", "output-equals"}),
		workflow.WithPreviewFeatures(true),
	)
	if err != nil {
		t.Fatalf("compile shipped implementation: %v", err)
	}

	instanceRoot := t.TempDir()
	wtMgr, err := worktree.NewManager(filepath.Join(instanceRoot, "workcopies"))
	if err != nil {
		t.Fatalf("new worktree manager: %v", err)
	}
	// The same fixture the rebinding tests use — it CONTAINS a second branch
	// (rebindBranch) precisely so that "every stage stayed on the run branch"
	// is a real assertion rather than a vacuous one on a single-branch repo.
	fixtureRepo := newRebindFixtureRepo(t)

	var mu sync.Mutex
	visited := []string{}
	branches := map[string]string{}

	byTask := map[string]stubTaskResult{
		runID + ":query-backlog": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"claimed-item": "42"}},
		runID + ":gather-implement-context": {
			status: apiv1.ResultSuccess, artifactName: "implementation-context.json",
			artifactData: []byte(`{"reviewerVerdictTaxonomy":{},"hotFileMap":{}}`), artifactMediaType: "application/json",
		},
		runID + ":local-ci":    {status: apiv1.ResultSuccess},
		runID + ":push-branch": {status: apiv1.ResultSuccess},
		runID + ":open-pr": {status: apiv1.ResultSuccess, outputs: map[string]interface{}{
			// #947: open-pr emits opened=true on the happy path (claimed issue
			// still open); open-pr-gate routes that to ci-poll.
			"prNumber": "101", "pull-request-url": "https://example.test/pr/101", "opened": "true",
		}},
		runID + ":ci-poll":   {status: apiv1.ResultSuccess, outputs: map[string]interface{}{"ciStatus": "passing"}},
		runID + ":close-out": {status: apiv1.ResultSuccess},
	}

	record := func(env apiv1.InvocationEnvelope) {
		_, stage, _ := strings.Cut(env.TaskID, ":")
		mu.Lock()
		defer mu.Unlock()
		visited = append(visited, stage)
		branches[stage] = currentBranch(t, env.Workspace)
	}

	r, err := New(Config{
		NewDeterministic: func(rec ArtifactRecorder, _ SecretRegistrar) (invoke.Deterministic, error) {
			return &recordingDeterministic{rec: rec, byTask: byTask, record: record}, nil
		},
		NewAgentic: func(string, ArtifactRecorder, SecretRegistrar) (invoke.Goober, error) {
			return &implementationGoober{t: t, record: record}, nil
		},
		Automated:              gate.NewAutomatedEvaluator(),
		GateGooberCapabilities: map[string][]string{"reviewer": {"agent:model"}},
		Worktrees:              wtMgr,
		RunsDir:                filepath.Join(instanceRoot, "runs"),
		RepoCloneURL:           func(apiv1.RepoRef) (string, error) { return fixtureRepo, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	res, err := r.Start(context.Background(), StartInput{
		RunID: runID, Machine: machine, Gaggle: "goobers",
		Trigger: journal.Trigger{Kind: journal.TriggerSchedule},
		RepoRef: apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res.Phase != journal.PhaseCompleted {
		t.Fatalf("phase = %q, want %q (visited: %v)", res.Phase, journal.PhaseCompleted, visited)
	}

	want := []string{"query-backlog", "gather-implement-context", "implement", "local-ci", "push-branch", "open-pr", "ci-poll", "close-out"}
	if strings.Join(visited, ",") != strings.Join(want, ",") {
		t.Errorf("stage order = %v, want %v", visited, want)
	}

	runBranch := providers.BranchName(machine.Def.Name, runID)
	for stage, branch := range branches {
		if branch != runBranch {
			t.Errorf("%s ran on branch %q, want the run's own branch %q — the #392 seam must not touch a workflow that never rebinds", stage, branch, runBranch)
		}
	}
}

// recordingDeterministic is stubDeterministic plus a caller-supplied observer,
// so one test can log both what ran and where it ran.
type recordingDeterministic struct {
	rec    ArtifactRecorder
	byTask map[string]stubTaskResult
	record func(apiv1.InvocationEnvelope)
}

func (d *recordingDeterministic) Run(ctx context.Context, env apiv1.InvocationEnvelope, dr apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	d.record(env)
	return (&stubDeterministic{rec: d.rec, byTask: d.byTask}).Run(ctx, env, dr)
}

// implementationGoober commits a real change (so the reviewer gate sees a
// non-empty diff, #415) and passes review.
type implementationGoober struct {
	t      *testing.T
	record func(apiv1.InvocationEnvelope)
}

func (g *implementationGoober) Invoke(_ context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.t.Helper()
	g.record(env)
	if err := os.WriteFile(filepath.Join(env.Workspace, "impl.txt"), []byte("implemented\n"), 0o644); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	runGit(g.t, env.Workspace, "add", "-A")
	runGit(g.t, env.Workspace, "commit", "-m", "implement the issue")
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "implemented"}, nil
}

func (g *implementationGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	return apiv1.Verdict{Decision: apiv1.VerdictPass, Rationale: "looks good"}, nil
}

// currentBranch reports the git branch a stage's workspace is checked out on.
func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("resolve branch in %s: %v", dir, err)
	}
	return strings.TrimSpace(string(out))
}
