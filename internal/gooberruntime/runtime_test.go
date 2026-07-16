package gooberruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

type fakePreparer struct {
	err error
	env ExecutionEnvironment
	got apiv1.InvocationEnvelope
}

func (f *fakePreparer) Prepare(_ context.Context, env apiv1.InvocationEnvelope) (ExecutionEnvironment, error) {
	f.got = env
	if f.err != nil {
		return ExecutionEnvironment{}, f.err
	}
	if f.env.WorkspaceDir == "" {
		f.env = ExecutionEnvironment{WorkspaceDir: "/workspace", RepoDir: "/workspace/repo", Env: map[string]string{"A": "B"}}
	}
	return f.env, nil
}

type fakeHarness struct {
	invokeResult  apiv1.ResultEnvelope
	reviewVerdict apiv1.Verdict
	err           error
	gotInvoke     HarnessRequest
	gotReview     HarnessRequest
}

func (f *fakeHarness) Invoke(_ context.Context, req HarnessRequest) (apiv1.ResultEnvelope, error) {
	f.gotInvoke = req
	if f.err != nil {
		return apiv1.ResultEnvelope{}, f.err
	}
	return f.invokeResult, nil
}

func (f *fakeHarness) Review(_ context.Context, req HarnessRequest) (apiv1.Verdict, error) {
	f.gotReview = req
	if f.err != nil {
		return apiv1.Verdict{}, f.err
	}
	return f.reviewVerdict, nil
}

type fakeEvaluator struct {
	verdict apiv1.Verdict
	err     error
	got     HarnessRequest
}

func (f *fakeEvaluator) Evaluate(_ context.Context, req HarnessRequest) (apiv1.Verdict, error) {
	f.got = req
	if f.err != nil {
		return apiv1.Verdict{}, f.err
	}
	return f.verdict, nil
}

func validInvocation() apiv1.InvocationEnvelope {
	return apiv1.InvocationEnvelope{
		TaskID:     "run-1:implement",
		WorkflowID: "default-implement",
		RunID:      "run-1",
		Gaggle:     "acme-web",
		Goal:       "Implement the backlog item",
		RepoRef:    apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Item:       &apiv1.BacklogItem{ID: "42", Provider: apiv1.ProviderGitHub, Title: "Fix bug"},
		Inputs: map[string]interface{}{
			"instructions": "You are a careful coder.",
			"draftPr":      true,
		},
		ContextPointers: []apiv1.ContextPointer{
			{Name: "plan", Artifact: &apiv1.ArtifactPointer{Path: "artifacts/plan/plan.md", Digest: apiv1.Digest([]byte("ready"))}},
		},
		Limits: apiv1.Limits{MaxDurationSeconds: 1800, MaxTokens: 1000, MaxCostUSD: 1.25},
	}
}

func TestInvokeBuildsContextAndReturnsResult(t *testing.T) {
	preparer := &fakePreparer{}
	harness := &fakeHarness{invokeResult: apiv1.ResultEnvelope{
		Status:  apiv1.ResultSuccess,
		Outputs: map[string]interface{}{"prNumber": float64(123)},
		Artifacts: []apiv1.ArtifactPointer{{
			Path:      "artifacts/implement/pr.json",
			Digest:    apiv1.Digest([]byte(`{"pr":123}`)),
			MediaType: "application/json",
		}},
		Summary: "opened PR",
	}}
	rt := New(Options{Preparer: preparer, Harness: harness, RequireInstructions: true})

	result, err := rt.Invoke(context.Background(), validInvocation())
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if harness.gotInvoke.Context.Instructions != "You are a careful coder." {
		t.Errorf("instructions = %q", harness.gotInvoke.Context.Instructions)
	}
	if harness.gotInvoke.Context.RepoRef.Name != "web" {
		t.Errorf("repo name = %q, want web", harness.gotInvoke.Context.RepoRef.Name)
	}
	if len(harness.gotInvoke.Context.ContextPointers) == 0 || harness.gotInvoke.Context.ContextPointers[0].Name != "plan" {
		t.Fatal("expected context pointer in harness context")
	}
	if harness.gotInvoke.Environment.RepoDir != "/workspace/repo" {
		t.Errorf("repo dir = %q", harness.gotInvoke.Environment.RepoDir)
	}
}

func TestInvokePropagatesHarnessError(t *testing.T) {
	rt := New(Options{
		Preparer: &fakePreparer{},
		Harness:  &fakeHarness{err: errors.New("boom")},
	})

	_, err := rt.Invoke(context.Background(), validInvocation())
	if err == nil || !strings.Contains(err.Error(), "invoke harness") {
		t.Fatalf("Invoke error = %v, want harness context", err)
	}
}

func TestInvokeRejectsInvalidResult(t *testing.T) {
	rt := New(Options{
		Preparer: &fakePreparer{},
		Harness:  &fakeHarness{invokeResult: apiv1.ResultEnvelope{Status: apiv1.ResultFailure}},
	})

	_, err := rt.Invoke(context.Background(), validInvocation())
	if err == nil || !strings.Contains(err.Error(), "requires error detail") {
		t.Fatalf("Invoke error = %v, want invalid result", err)
	}
}

func TestInvokeRequiresValidEnvelope(t *testing.T) {
	rt := New(Options{Preparer: &fakePreparer{}, Harness: &fakeHarness{}})
	env := validInvocation()
	env.TaskID = ""

	_, err := rt.Invoke(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "taskId is required") {
		t.Fatalf("Invoke error = %v, want taskId validation", err)
	}
}

func TestInputInstructionResolverReadsRootedInstructions(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "instructions.md"), []byte("be precise"), 0o600); err != nil {
		t.Fatalf("write instructions: %v", err)
	}
	env := validInvocation()
	env.Inputs = map[string]interface{}{"instructionsPath": "instructions.md"}
	resolver := InputInstructionResolver{InstructionsRoot: root}

	got, err := resolver.ResolveInstructions(context.Background(), env)
	if err != nil {
		t.Fatalf("ResolveInstructions returned error: %v", err)
	}
	if got != "be precise" {
		t.Fatalf("instructions = %q, want be precise", got)
	}
}

func TestInputInstructionResolverRejectsUntrustedPath(t *testing.T) {
	env := validInvocation()
	env.Inputs = map[string]interface{}{"instructionsPath": "../secret.txt"}
	resolver := InputInstructionResolver{InstructionsRoot: t.TempDir()}

	_, err := resolver.ResolveInstructions(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "within instructions root") {
		t.Fatalf("ResolveInstructions error = %v, want root confinement error", err)
	}
}

func TestReviewUsesEvaluatorAndReturnsVerdict(t *testing.T) {
	evaluator := &fakeEvaluator{verdict: apiv1.Verdict{
		Decision: apiv1.VerdictNeedsChanges,
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError,
			Message:  "missing tests",
			Location: "foo_test.go",
		}},
		Summary: "fix tests",
	}}
	rt := New(Options{Preparer: &fakePreparer{}, Evaluator: evaluator})

	verdict, err := rt.Review(context.Background(), validInvocation())
	if err != nil {
		t.Fatalf("Review returned error: %v", err)
	}
	if verdict.Decision != apiv1.VerdictNeedsChanges {
		t.Fatalf("decision = %q, want needs-changes", verdict.Decision)
	}
	if len(evaluator.got.Context.ContextPointers) == 0 || evaluator.got.Context.ContextPointers[0].Name != "plan" {
		t.Fatal("expected context pointer in evaluator context")
	}
}

func TestReviewFallsBackToHarnessEvaluator(t *testing.T) {
	harness := &fakeHarness{reviewVerdict: apiv1.Verdict{Decision: apiv1.VerdictPass}}
	rt := New(Options{Preparer: &fakePreparer{}, Harness: harness})

	verdict, err := rt.Review(context.Background(), validInvocation())
	if err != nil {
		t.Fatalf("Review returned error: %v", err)
	}
	if verdict.Decision != apiv1.VerdictPass {
		t.Fatalf("decision = %q, want pass", verdict.Decision)
	}
	if harness.gotReview.Context.Goal != "Implement the backlog item" {
		t.Errorf("goal = %q", harness.gotReview.Context.Goal)
	}
}

func TestReviewRejectsInvalidVerdict(t *testing.T) {
	rt := New(Options{
		Preparer:  &fakePreparer{},
		Evaluator: &fakeEvaluator{verdict: apiv1.Verdict{Decision: "maybe"}},
	})

	_, err := rt.Review(context.Background(), validInvocation())
	if err == nil || !strings.Contains(err.Error(), "invalid verdict decision") {
		t.Fatalf("Review error = %v, want invalid verdict", err)
	}
}

type fakeRepoProvider struct {
	got providers.CloneRequest
	err error
}

func (f *fakeRepoProvider) CloneRepository(_ context.Context, req providers.CloneRequest) (providers.CloneResult, error) {
	f.got = req
	if f.err != nil {
		return providers.CloneResult{}, f.err
	}
	return providers.CloneResult{Path: req.Destination, URL: "https://example.test/repo.git"}, nil
}

func (f *fakeRepoProvider) CreateBranch(context.Context, providers.BranchRequest) (providers.BranchResult, error) {
	return providers.BranchResult{}, nil
}

func (f *fakeRepoProvider) DeleteBranch(context.Context, providers.DeleteBranchRequest) (providers.DeleteBranchResult, error) {
	return providers.DeleteBranchResult{}, nil
}

func (f *fakeRepoProvider) Commit(context.Context, providers.CommitRequest) (providers.CommitResult, error) {
	return providers.CommitResult{}, nil
}

func (f *fakeRepoProvider) OpenPullRequest(context.Context, providers.PullRequestRequest) (providers.PullRequestResult, error) {
	return providers.PullRequestResult{}, nil
}

func (f *fakeRepoProvider) RequestReview(context.Context, providers.ReviewRequest) error {
	return nil
}

func (f *fakeRepoProvider) PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	return providers.PullRequestPollResult{}, nil
}

func (f *fakeRepoProvider) ClosePullRequest(context.Context, providers.ClosePullRequestRequest) (providers.ClosePullRequestResult, error) {
	return providers.ClosePullRequestResult{}, nil
}

func (f *fakeRepoProvider) MergePullRequest(context.Context, providers.MergePullRequestRequest) (providers.MergePullRequestResult, error) {
	return providers.MergePullRequestResult{}, nil
}

func (f *fakeRepoProvider) ListPullRequests(context.Context, providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error) {
	return nil, nil
}

func (f *fakeRepoProvider) PullRequestFiles(context.Context, providers.RepositoryRef, string) ([]providers.ChangedFile, error) {
	return nil, nil
}

func (f *fakeRepoProvider) CompareCommits(context.Context, providers.RepositoryRef, string, string) (providers.CompareResult, error) {
	return providers.CompareResult{}, nil
}

func TestInProcessPreparerClonesRepoAndBuildsEnv(t *testing.T) {
	repo := &fakeRepoProvider{}
	preparer := InProcessPreparer{
		WorkspaceRoot: t.TempDir(),
		Providers: StaticProviderResolver{
			apiv1.ProviderGitHub: repo,
		},
		Env: map[string]string{"EXTRA": "1"},
	}

	execEnv, err := preparer.Prepare(context.Background(), validInvocation())
	if err != nil {
		t.Fatalf("Prepare returned error: %v", err)
	}
	if repo.got.Repository.Owner != "acme" || repo.got.Repository.Name != "web" {
		t.Fatalf("repo request = %+v, want acme/web", repo.got.Repository)
	}
	if repo.got.Branch != "main" {
		t.Errorf("branch = %q, want main", repo.got.Branch)
	}
	if !strings.HasPrefix(execEnv.WorkspaceDir, preparer.WorkspaceRoot) {
		t.Errorf("workspace = %q, want under %q", execEnv.WorkspaceDir, preparer.WorkspaceRoot)
	}
	if execEnv.RepoDir != filepath.Join(execEnv.WorkspaceDir, "repo") {
		t.Errorf("repo dir = %q, want workspace/repo", execEnv.RepoDir)
	}
	if execEnv.Env["GOOBERS_RUN_ID"] != "run-1" || execEnv.Env["EXTRA"] != "1" {
		t.Errorf("env = %+v", execEnv.Env)
	}
}

func TestInProcessPreparerReportsProviderErrors(t *testing.T) {
	preparer := InProcessPreparer{WorkspaceRoot: t.TempDir(), Providers: StaticProviderResolver{}}

	_, err := preparer.Prepare(context.Background(), validInvocation())
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("Prepare error = %v, want provider config error", err)
	}
}

func TestEnvProviderResolverBuildsConfiguredProviders(t *testing.T) {
	for _, key := range []string{
		"GOOBERS_GITHUB_TOKEN", "GITHUB_TOKEN",
		"GOOBERS_ADO_TOKEN", "AZURE_DEVOPS_TOKEN", "ADO_TOKEN",
		"GOOBERS_ADO_ORG", "AZURE_DEVOPS_ORG", "ADO_ORG",
		"GOOBERS_ADO_PROJECT", "AZURE_DEVOPS_PROJECT", "ADO_PROJECT",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("GOOBERS_GITHUB_TOKEN", "github-token")
	t.Setenv("AZURE_DEVOPS_TOKEN", "ado-token")
	t.Setenv("AZURE_DEVOPS_ORG", "ado-org")
	t.Setenv("AZURE_DEVOPS_PROJECT", "ado-project")
	resolver := EnvProviderResolver{}

	githubProvider, err := resolver.RepoProvider(apiv1.ProviderGitHub, apiv1.RepoRef{})
	if err != nil {
		t.Fatalf("RepoProvider(GitHub) returned error: %v", err)
	}
	if githubProvider == nil {
		t.Fatal("RepoProvider(GitHub) returned nil provider")
	}
	adoProvider, err := resolver.RepoProvider(apiv1.ProviderADO, apiv1.RepoRef{})
	if err != nil {
		t.Fatalf("RepoProvider(ADO) returned error: %v", err)
	}
	if adoProvider == nil {
		t.Fatal("RepoProvider(ADO) returned nil provider")
	}
}

func TestEnvProviderResolverReportsConfigurationErrors(t *testing.T) {
	for _, key := range []string{
		"GOOBERS_GITHUB_TOKEN", "GITHUB_TOKEN",
		"GOOBERS_ADO_TOKEN", "AZURE_DEVOPS_TOKEN", "ADO_TOKEN",
		"GOOBERS_ADO_ORG", "AZURE_DEVOPS_ORG", "ADO_ORG",
		"GOOBERS_ADO_PROJECT", "AZURE_DEVOPS_PROJECT", "ADO_PROJECT",
	} {
		t.Setenv(key, "")
	}
	resolver := EnvProviderResolver{}

	if _, err := resolver.RepoProvider(apiv1.ProviderGitHub, apiv1.RepoRef{}); err == nil {
		t.Fatal("RepoProvider(GitHub) error = nil, want missing token error")
	}
	if _, err := resolver.RepoProvider(apiv1.ProviderADO, apiv1.RepoRef{}); err == nil {
		t.Fatal("RepoProvider(ADO) error = nil, want missing token error")
	}
	if _, err := resolver.RepoProvider(apiv1.Provider("gitlab"), apiv1.RepoRef{}); err == nil {
		t.Fatal("RepoProvider(unsupported) error = nil, want unsupported provider error")
	}
}

func TestKubernetesPreparerReportsNotImplemented(t *testing.T) {
	_, err := KubernetesPreparer{}.Prepare(context.Background(), validInvocation())
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("Prepare error = %v, want not implemented", err)
	}
}

type captureRunner struct {
	got ProcessRequest
	out []byte
	err error
}

func (r *captureRunner) Run(_ context.Context, req ProcessRequest) ([]byte, error) {
	r.got = req
	return r.out, r.err
}

func TestCopilotHarnessRunsInvokeCommand(t *testing.T) {
	runner := &captureRunner{out: []byte(`{"status":"success","summary":"done"}`)}
	h := &CopilotHarness{Command: []string{"copilot-harness"}, Runner: runner}
	req := HarnessRequest{
		Context:     GooberContext{TaskID: "task"},
		Environment: ExecutionEnvironment{RepoDir: "/repo", Env: map[string]string{"X": "Y"}},
	}

	result, err := h.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke returned error: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %q, want success", result.Status)
	}
	if !reflect.DeepEqual(runner.got.Command, []string{"copilot-harness", "invoke"}) {
		t.Fatalf("command = %v", runner.got.Command)
	}
	if runner.got.Dir != "/repo" || runner.got.Env["X"] != "Y" {
		t.Fatalf("process request = %+v", runner.got)
	}
}

func TestCopilotHarnessRunsReviewCommand(t *testing.T) {
	runner := &captureRunner{out: []byte(`{"decision":"pass","summary":"approved"}`)}
	h := &CopilotHarness{Command: []string{"copilot-harness"}, Runner: runner}
	req := HarnessRequest{
		Context:     GooberContext{TaskID: "review-task"},
		Environment: ExecutionEnvironment{RepoDir: "/repo", Env: map[string]string{"X": "Y"}},
	}

	verdict, err := h.Review(context.Background(), req)
	if err != nil {
		t.Fatalf("Review returned error: %v", err)
	}
	if verdict.Decision != apiv1.VerdictPass {
		t.Fatalf("decision = %q, want pass", verdict.Decision)
	}
	if !reflect.DeepEqual(runner.got.Command, []string{"copilot-harness", "review"}) {
		t.Fatalf("command = %v", runner.got.Command)
	}
	if runner.got.Dir != "/repo" || runner.got.Env["X"] != "Y" {
		t.Fatalf("process request = %+v", runner.got)
	}
}

func TestCopilotHarnessReviewReportsRunnerErrors(t *testing.T) {
	runErr := errors.New("runner failed")
	h := &CopilotHarness{
		Command: []string{"copilot-harness"},
		Runner:  &captureRunner{err: runErr},
	}

	_, err := h.Review(context.Background(), HarnessRequest{})
	if !errors.Is(err, runErr) {
		t.Fatalf("error = %v, want runner failure", err)
	}
}

func TestNewCopilotHarnessCopiesCommandAndDefaultsRunner(t *testing.T) {
	command := []string{"copilot-harness", "--json"}
	h := NewCopilotHarness(command)
	command[0] = "mutated"

	if !reflect.DeepEqual(h.Command, []string{"copilot-harness", "--json"}) {
		t.Fatalf("command = %v, want copied command", h.Command)
	}
	if h.Runner == nil {
		t.Fatal("Runner = nil, want default process runner")
	}
}

func TestCopilotHarnessRequiresCommand(t *testing.T) {
	_, err := (&CopilotHarness{}).Invoke(context.Background(), HarnessRequest{})
	if !errors.Is(err, ErrHarnessUnavailable) {
		t.Fatalf("error = %v, want ErrHarnessUnavailable", err)
	}
}

func TestHarnessEvaluatorRequiresHarness(t *testing.T) {
	_, err := HarnessEvaluator{}.Evaluate(context.Background(), HarnessRequest{})
	if !errors.Is(err, ErrHarnessUnavailable) {
		t.Fatalf("error = %v, want ErrHarnessUnavailable", err)
	}
}
