package tutor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/providers"
)

func TestRunnerOpensPRForGateRejectionTelemetry(t *testing.T) {
	ctx := context.Background()
	exporter := telemetry.NewMemoryExporter()
	client, err := telemetry.New(ctx, telemetry.Config{
		ServiceName:  "tutor-test",
		SpanExporter: exporter,
	})
	if err != nil {
		t.Fatalf("telemetry.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	})

	decisions := []string{"needs-changes", "fail", "pass", "fail"}
	for i, decision := range decisions {
		runID := []string{
			"0af7651916cd43dd8448eb211c80319c",
			"1af7651916cd43dd8448eb211c80319c",
			"2af7651916cd43dd8448eb211c80319c",
			"3af7651916cd43dd8448eb211c80319c",
		}[i]
		runCtx, runSpan, err := client.StartRun(ctx, telemetry.RunAttributes{
			Gaggle:     "acme-web",
			WorkflowID: "default-implement",
			RunID:      runID,
			Trigger:    "schedule",
		})
		if err != nil {
			t.Fatalf("StartRun() error = %v", err)
		}
		_, gateSpan, err := client.StartGate(runCtx, telemetry.GateAttributes{
			Gaggle:     "acme-web",
			WorkflowID: "default-implement",
			RunID:      runID,
			GateID:     "qa",
			Evaluator:  "agentic",
			Decision:   decision,
			GooberID:   "qa-reviewer",
		})
		if err != nil {
			t.Fatalf("StartGate() error = %v", err)
		}
		gateSpan.End()
		runSpan.End()
	}
	if err := client.Flush(ctx); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}

	provider := &mockRepoProvider{
		pr: providers.PullRequestResult{ID: "17", Number: 17, URL: "https://example.test/pr/17"},
		existingFiles: map[string]string{
			"gaggles/acme-web/goobers/qa-reviewer/instructions.md": "# QA reviewer\nReview changes carefully.\n",
		},
	}
	result, err := Runner{
		Store:    NewSpanStore(exporter.Spans()),
		Provider: provider,
		Config: RunnerConfig{
			Repository:   providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "config"},
			BaseBranch:   "main",
			BranchPrefix: "Goober-Dev-4/tutor",
			Reviewers:    []string{"qa-2"},
		},
	}.Run(ctx)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !result.Opened || result.PullRequest == nil || result.PullRequest.Number != 17 {
		t.Fatalf("Run() result = %#v, want opened PR #17", result)
	}
	if len(result.Findings) == 0 || result.Findings[0].Type != FindingGateRejection {
		t.Fatalf("findings = %#v, want gate rejection first", result.Findings)
	}
	if provider.branch.Name != "Goober-Dev-4/tutor/gate-rejection-default-implement-qa" {
		t.Fatalf("branch name = %q", provider.branch.Name)
	}
	if provider.prRequest.Head != provider.branch.Name || provider.prRequest.Base != "main" {
		t.Fatalf("PR refs = head %q base %q", provider.prRequest.Head, provider.prRequest.Base)
	}
	if len(provider.commit.Files) != 1 {
		t.Fatalf("committed files = %#v", provider.commit.Files)
	}
	file := provider.commit.Files[0]
	if file.Path != "gaggles/acme-web/goobers/qa-reviewer/instructions.md" {
		t.Fatalf("proposal path = %q", file.Path)
	}
	if file.ChangeType != string(providers.CommitChangeEdit) {
		t.Fatalf("proposal change type = %q", file.ChangeType)
	}
	for _, want := range []string{"# QA reviewer", "Review changes carefully.", "Tutor-added guidance", "Gate \"qa\"", "3 of 4", "Telemetry evidence"} {
		if !strings.Contains(file.Content, want) {
			t.Fatalf("proposal content missing %q:\n%s", want, file.Content)
		}
	}
	if !strings.Contains(provider.prRequest.Body, "human-readable") && !strings.Contains(provider.prRequest.Body, "Gate \"qa\"") {
		t.Fatalf("PR body lacks rationale: %s", provider.prRequest.Body)
	}
	if strings.Join(provider.review.Reviewers, ",") != "qa-2" || provider.review.PullID != "17" {
		t.Fatalf("review request = %#v", provider.review)
	}
}

func TestRunnerDoesNotOpenPRWithoutSignals(t *testing.T) {
	provider := &mockRepoProvider{}
	result, err := Runner{
		Store:    StaticStore(nil),
		Provider: provider,
		Config: RunnerConfig{
			Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "config"},
		},
	}.Run(context.Background())
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Opened || provider.calls != 0 {
		t.Fatalf("Run() opened = %v, provider calls = %d; want no PR", result.Opened, provider.calls)
	}
}

func TestAnalyzerAndPlannerTargetGooberForRetryHeavyFailures(t *testing.T) {
	signals := []Signal{
		taskSignal("run-1", statusError, 11*time.Minute, 3),
		taskSignal("run-2", statusError, 12*time.Minute, 4),
		taskSignal("run-3", statusError, 13*time.Minute, 5),
	}
	findings := NewAnalyzer(Thresholds{}).Analyze(signals)
	if len(findings) < 3 {
		t.Fatalf("findings = %#v, want task failure, retry, and slow-task findings", findings)
	}
	var retry Finding
	for _, finding := range findings {
		if finding.Type == FindingRetries {
			retry = finding
			break
		}
	}
	if retry.Type != FindingRetries {
		t.Fatalf("findings = %#v, want retry finding", findings)
	}
	if retry.GooberID != "coder" || retry.ProblemCount != 3 {
		t.Fatalf("retry finding = %#v", retry)
	}
	proposal := Planner{BranchPrefix: "tutor"}.Propose(retry)
	if proposal.Files[0].Path != "gaggles/acme-web/goobers/coder/instructions.md" {
		t.Fatalf("proposal path = %q", proposal.Files[0].Path)
	}
	if !strings.Contains(proposal.Body, "retry-heavy execution") {
		t.Fatalf("proposal body = %s", proposal.Body)
	}
}

func TestSpanRecorderIncludesRationale(t *testing.T) {
	ctx := context.Background()
	exporter := telemetry.NewMemoryExporter()
	client, err := telemetry.New(ctx, telemetry.Config{ServiceName: "tutor-test", SpanExporter: exporter})
	if err != nil {
		t.Fatalf("telemetry.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown() error = %v", err)
		}
	})
	_, span, err := client.StartSchedulerSpan(ctx, telemetry.SchedulerAttributes{
		Gaggle:     "acme-web",
		WorkflowID: "tutor",
		Action:     "analyze",
	})
	if err != nil {
		t.Fatalf("StartSchedulerSpan() error = %v", err)
	}
	finding := Finding{
		Type:           FindingTaskFailure,
		Severity:       "medium",
		WorkflowID:     "default-implement",
		TaskID:         "implement",
		ProblemCount:   3,
		Rationale:      "Task \"implement\" failed 3 of 4 recent executions.",
		Recommendation: "Add diagnostics guidance.",
	}
	SpanRecorder{Span: span}.RecordProposal(ctx, Proposal{Finding: finding}, providers.PullRequestResult{ID: "9", URL: "pr-url"})
	span.End()
	if err := client.Flush(ctx); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	spans := exporter.Spans()
	if len(spans) != 1 || len(spans[0].Events()) != 1 {
		t.Fatalf("spans/events = %#v", spans)
	}
	attrs := map[string]string{}
	for _, attr := range spans[0].Events()[0].Attributes {
		attrs[string(attr.Key)] = attr.Value.AsString()
	}
	if attrs["tutor.rationale"] != finding.Rationale || attrs["tutor.recommendation"] != finding.Recommendation {
		t.Fatalf("recorded attrs = %#v", attrs)
	}
}

func TestRunnerPropagatesProviderErrors(t *testing.T) {
	provider := &mockRepoProvider{err: errors.New("provider down")}
	_, err := Runner{
		Store: StaticStore{
			{Kind: SignalGate, Gaggle: "acme", WorkflowID: "wf", GateID: "qa", Decision: "fail"},
			{Kind: SignalGate, Gaggle: "acme", WorkflowID: "wf", GateID: "qa", Decision: "fail"},
			{Kind: SignalGate, Gaggle: "acme", WorkflowID: "wf", GateID: "qa", Decision: "pass"},
		},
		Provider: provider,
		Config: RunnerConfig{
			Repository: providers.RepositoryRef{Provider: providers.ProviderGitHub, Owner: "acme", Name: "config"},
		},
	}.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "clone config repository") {
		t.Fatalf("Run() error = %v, want clone error", err)
	}
}

func taskSignal(runID, status string, duration time.Duration, retries int) Signal {
	return Signal{
		Kind:       SignalTask,
		Gaggle:     "acme-web",
		WorkflowID: "default-implement",
		RunID:      runID,
		TaskID:     "implement",
		GooberID:   "coder",
		Status:     status,
		Duration:   duration,
		RetryCount: retries,
	}
}

type mockRepoProvider struct {
	err           error
	pr            providers.PullRequestResult
	calls         int
	existingFiles map[string]string
	branch        providers.BranchRequest
	commit        providers.CommitRequest
	prRequest     providers.PullRequestRequest
	review        providers.ReviewRequest
}

func (m *mockRepoProvider) CloneRepository(_ context.Context, req providers.CloneRequest) (providers.CloneResult, error) {
	m.calls++
	if m.err != nil {
		return providers.CloneResult{}, m.err
	}
	for rel, content := range m.existingFiles {
		path := filepath.Join(req.Destination, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return providers.CloneResult{}, err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return providers.CloneResult{}, err
		}
	}
	return providers.CloneResult{Path: req.Destination}, nil
}

func (m *mockRepoProvider) CreateBranch(_ context.Context, req providers.BranchRequest) (providers.BranchResult, error) {
	m.calls++
	m.branch = req
	if m.err != nil {
		return providers.BranchResult{}, m.err
	}
	return providers.BranchResult{Name: req.Name, SHA: "branch-sha"}, nil
}

func (m *mockRepoProvider) Commit(_ context.Context, req providers.CommitRequest) (providers.CommitResult, error) {
	m.calls++
	m.commit = req
	if m.err != nil {
		return providers.CommitResult{}, m.err
	}
	return providers.CommitResult{SHA: "commit-sha", URL: "commit-url"}, nil
}

func (m *mockRepoProvider) OpenPullRequest(_ context.Context, req providers.PullRequestRequest) (providers.PullRequestResult, error) {
	m.calls++
	m.prRequest = req
	if m.err != nil {
		return providers.PullRequestResult{}, m.err
	}
	if m.pr.ID == "" && m.pr.Number == 0 {
		m.pr = providers.PullRequestResult{ID: "1", Number: 1, URL: "pr-url"}
	}
	return m.pr, nil
}

func (m *mockRepoProvider) RequestReview(_ context.Context, req providers.ReviewRequest) error {
	m.calls++
	m.review = req
	return m.err
}

func (m *mockRepoProvider) PollPullRequest(_ context.Context, req providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	m.calls++
	return providers.PullRequestPollResult{}, m.err
}

func (m *mockRepoProvider) ClosePullRequest(_ context.Context, req providers.ClosePullRequestRequest) (providers.ClosePullRequestResult, error) {
	m.calls++
	return providers.ClosePullRequestResult{}, m.err
}
