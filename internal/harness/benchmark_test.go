package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/providerfixture"
	"github.com/goobers/goobers/internal/workflowgym"
	"github.com/goobers/goobers/providers"
)

const benchmarkTestStage = "implement"

var benchmarkTestRepo = providers.RepositoryRef{Owner: "acme", Name: "web"}

// recordBenchmarkJournal writes a recorded stage journal with the production
// journal writer and returns its directory.
func recordBenchmarkJournal(t *testing.T, failure *journal.ErrorDetail) string {
	t.Helper()
	run, err := journal.Create(t.TempDir(), journal.RunIdentity{
		RunID:           "0af7651916cd43dd8448eb211c80319c",
		Workflow:        "default-implement",
		WorkflowVersion: 1,
		Gaggle:          "example",
		Trigger:         journal.Trigger{Kind: journal.TriggerItem, Ref: "7"},
	}, nil)
	if err != nil {
		t.Fatalf("journal.Create: %v", err)
	}
	defer func() { _ = run.Close() }()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(run.Append(journal.Event{Type: journal.EventStageStarted, Stage: benchmarkTestStage, Attempt: 1}))
	completion, err := json.Marshal(apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "recorded fix"})
	must(err)
	for _, artifact := range []struct {
		name string
		data []byte
	}{
		{BenchmarkTranscriptArtifact, []byte(`{"turn":1,"text":"reading the issue"}` + "\n")},
		{"diff.patch", []byte("--- a/login_test.go\n+++ b/login_test.go\n")},
		{BenchmarkCompletionArtifact, completion},
	} {
		_, err := run.RecordStageArtifact(benchmarkTestStage, 1, journal.AttemptPolicy, artifact.name, artifact.data)
		must(err)
	}
	if failure != nil {
		must(run.Append(journal.Event{Type: journal.EventError, Stage: benchmarkTestStage, Attempt: 1, Error: failure}))
	}
	must(run.Append(journal.Event{
		Type: journal.EventStageFinished, Stage: benchmarkTestStage, Attempt: 1, Status: "success",
		Outputs: map[string]any{"costUsd": 0.42, "tokens": 1234, "summary": "not a metric"},
	}))
	return run.Dir()
}

func loadBenchmarkTestFixture(t *testing.T, failure *journal.ErrorDetail) BenchmarkFixture {
	t.Helper()
	fixture, err := LoadBenchmarkFixture(filepath.Join("testdata", "workflowgym", "provider.json"), recordBenchmarkJournal(t, failure))
	if err != nil {
		t.Fatalf("LoadBenchmarkFixture: %v", err)
	}
	return fixture
}

func newBenchmarkTestSandbox(t *testing.T, fixture BenchmarkFixture) *BenchmarkSandbox {
	t.Helper()
	sandbox, err := NewBenchmarkSandbox(fixture)
	if err != nil {
		t.Fatalf("NewBenchmarkSandbox: %v", err)
	}
	return sandbox
}

func benchmarkRunRequest(workspace string) RunRequest {
	return RunRequest{Envelope: testEnvelope(workspace), Workspace: workspace, CompletionPath: DefaultResultPath}
}

// triageAct is a scripted benchmark agent that reads its issue and comments
// through the production GitHub provider wired to the sandbox seams.
func triageAct(ctx context.Context, _ RunRequest, sandbox *BenchmarkSandbox) error {
	provider, err := sandbox.GitHubProvider()
	if err != nil {
		return err
	}
	if _, err := provider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
		Repository: benchmarkTestRepo, State: "open", OldestFirst: true, Limit: 100, Page: 1,
	}); err != nil {
		return err
	}
	if _, err := provider.GetWorkItem(ctx, benchmarkTestRepo, "7"); err != nil {
		return err
	}
	_, err = provider.CreateWorkItemComment(ctx, benchmarkTestRepo, "7", "analysis")
	return err
}

func runBenchmark(t *testing.T, fixture BenchmarkFixture, act BenchmarkAct) (Outcome, BenchmarkReport, error) {
	t.Helper()
	sandbox := newBenchmarkTestSandbox(t, fixture)
	out, runErr := sandbox.Adapter(act).Run(context.Background(), benchmarkRunRequest(t.TempDir()))
	report, err := sandbox.Report()
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	return out, report, runErr
}

func TestBenchmarkReplayIsByteStable(t *testing.T) {
	fixture := loadBenchmarkTestFixture(t, nil)
	first, firstReport, err := runBenchmark(t, fixture, triageAct)
	if err != nil {
		t.Fatalf("first Run: %v", err)
	}
	second, secondReport, err := runBenchmark(t, loadBenchmarkTestFixture(t, nil), triageAct)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if !bytes.Equal(firstReport.Journal, secondReport.Journal) {
		t.Fatalf("journal is not byte-stable:\n%s\n---\n%s", firstReport.Journal, secondReport.Journal)
	}
	if !bytes.Equal(first.Payload, second.Payload) || !bytes.Equal(first.Transcript, second.Transcript) {
		t.Fatal("replayed transcript/payload are not byte-stable")
	}
	if firstReport.Status != "success" || !strings.Contains(string(first.Payload), "recorded fix") {
		t.Fatalf("status %q payload %s, want recorded success", firstReport.Status, first.Payload)
	}
	if !strings.Contains(string(first.Transcript), "reading the issue") {
		t.Fatalf("transcript = %q, want recorded transcript", first.Transcript)
	}
	if first.Metrics["costUsd"] != 0.42 || first.Metrics["tokens"] != 1234 || len(first.Metrics) != 2 {
		t.Fatalf("metrics = %v, want recorded numeric outputs", first.Metrics)
	}
	var artifacts []string
	for _, event := range firstReport.Events {
		if event.Type == journal.EventArtifactRecorded {
			artifacts = append(artifacts, event.Name)
		}
	}
	if got := strings.Join(artifacts, ","); got != "transcript.jsonl,diff.patch,completion.json" {
		t.Fatalf("replayed artifacts = %s", got)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(firstReport.Journal), []byte("\n")) {
		var event journal.Event
		if err := json.Unmarshal(line, &event); err != nil || event.Schema != journal.EventSchema {
			t.Fatalf("journal line %s does not decode as a journal event: %v", line, err)
		}
	}
}

func TestBenchmarkChecksGeneratedTaskFailureModes(t *testing.T) {
	for _, class := range workflowgym.TaskClasses() {
		t.Run(string(class), func(t *testing.T) {
			task, err := workflowgym.Generate(workflowgym.GeneratorVersionV1, class, 8225)
			if err != nil {
				t.Fatal(err)
			}
			var recorded *journal.ErrorDetail
			var act BenchmarkAct
			switch class {
			case workflowgym.TaskClassNormal:
			case workflowgym.TaskClassMalicious:
				act = func(ctx context.Context, _ RunRequest, sandbox *BenchmarkSandbox) error {
					_, err := sandbox.Token(ctx)
					return err
				}
			case workflowgym.TaskClassAdversarial:
				act = func(ctx context.Context, _ RunRequest, sandbox *BenchmarkSandbox) error {
					req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/repos/acme/web/issues/7/comments", nil)
					if err != nil {
						return err
					}
					_, err = sandbox.Do(req)
					return err
				}
			default:
				recorded = &journal.ErrorDetail{Code: task.ExpectedFailureMode, Message: "synthetic failure"}
			}
			_, _, runErr := runBenchmark(t, loadBenchmarkTestFixture(t, recorded), act)
			if err := CheckBenchmarkExpectedFailure(task, runErr); err != nil {
				t.Error(err)
			}
		})
	}
}

func TestBenchmarkRejectsMissingAndMismatchedExpectedFailures(t *testing.T) {
	task, err := workflowgym.Generate(workflowgym.GeneratorVersionV1, workflowgym.TaskClassTimeout, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckBenchmarkExpectedFailure(task, nil); err == nil {
		t.Fatal("missing expected failure passed")
	}
	wrong := &BenchmarkRecordedFailure{Detail: journal.ErrorDetail{Code: "wrong_failure", Message: "wrong"}}
	if err := CheckBenchmarkExpectedFailure(task, wrong); err == nil {
		t.Fatal("mismatched expected failure passed")
	}
}

func TestBenchmarkProviderCallsAreInterceptedAndVirtualized(t *testing.T) {
	_, report, err := runBenchmark(t, loadBenchmarkTestFixture(t, nil), triageAct)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(report.Receipts) != 1 || report.Receipts[0].Effect != BenchmarkEffectComment ||
		report.Receipts[0].Exchange != "comment-on-issue" {
		t.Fatalf("receipts = %+v, want the comment served by its recorded sink", report.Receipts)
	}
	var touched *journal.ExternalRef
	for _, event := range report.Events {
		if event.Type == journal.EventRefTouched {
			touched = event.ExternalRef
		}
	}
	if touched == nil || touched.Kind != "comment" || touched.Provider != "github" {
		t.Fatalf("ref.touched = %+v, want the provider's comment fact", touched)
	}
}

func TestBenchmarkProductionMutationsFailClosed(t *testing.T) {
	cases := map[string]struct {
		act    func(context.Context, *BenchmarkSandbox) error
		effect BenchmarkEffect
	}{
		"merge": {effect: BenchmarkEffectMerge, act: func(ctx context.Context, s *BenchmarkSandbox) error {
			provider, err := s.GitHubProvider()
			if err != nil {
				return err
			}
			_, err = provider.MergePullRequest(ctx, providers.MergePullRequestRequest{Repository: benchmarkTestRepo, PullID: "8"})
			return err
		}},
		"second comment": {effect: BenchmarkEffectComment, act: func(ctx context.Context, s *BenchmarkSandbox) error {
			provider, err := s.GitHubProvider()
			if err != nil {
				return err
			}
			if _, err := provider.CreateWorkItemComment(ctx, benchmarkTestRepo, "7", "one"); err != nil {
				return err
			}
			_, err = provider.CreateWorkItemComment(ctx, benchmarkTestRepo, "7", "two")
			return err
		}},
		"credential read": {effect: BenchmarkEffectCredentialRead, act: func(ctx context.Context, s *BenchmarkSandbox) error {
			_, err := providers.TokenSource(s).Token(ctx)
			return err
		}},
		"git push": {effect: BenchmarkEffectGitPush, act: func(ctx context.Context, s *BenchmarkSandbox) error {
			_, err := providers.CommandRunner(s).Run(ctx, "git", "push", "origin", "HEAD:main")
			return err
		}},
		"real credential on provider call": {effect: BenchmarkEffectCredentialRead, act: func(ctx context.Context, s *BenchmarkSandbox) error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, BenchmarkProviderBaseURL+"/repos/acme/web/issues/7", nil)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer ghp_real")
			_, err = providers.HTTPClient(s).Do(req)
			return err
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var actErr error
			out, report, err := runBenchmark(t, loadBenchmarkTestFixture(t, nil),
				func(ctx context.Context, _ RunRequest, s *BenchmarkSandbox) error {
					actErr = tc.act(ctx, s)
					return nil // a misbehaving agent swallows the denial
				})
			if !errors.Is(actErr, ErrBenchmarkMutationDenied) {
				t.Fatalf("act error = %v, want ErrBenchmarkMutationDenied", actErr)
			}
			var violation *BenchmarkViolation
			if !errors.As(err, &violation) || violation.Effect != tc.effect {
				t.Fatalf("Run error = %v, want fail-closed %s violation", err, tc.effect)
			}
			if out.Payload != nil || report.Status != "failure" {
				t.Fatalf("payload %s status %q, want no completion and failure", out.Payload, report.Status)
			}
			if !bytes.Contains(report.Journal, []byte(BenchmarkErrorMutationDenied)) {
				t.Fatalf("journal does not report the denial:\n%s", report.Journal)
			}
		})
	}
}

func TestBenchmarkVirtualizationGapsAreReported(t *testing.T) {
	t.Run("unrecorded provider read", func(t *testing.T) {
		_, report, err := runBenchmark(t, loadBenchmarkTestFixture(t, nil),
			func(ctx context.Context, _ RunRequest, s *BenchmarkSandbox) error {
				provider, err := s.GitHubProvider()
				if err != nil {
					return err
				}
				_, err = provider.GetWorkItem(ctx, benchmarkTestRepo, "99")
				return err
			})
		if !errors.Is(err, ErrBenchmarkVirtualizationGap) {
			t.Fatalf("Run error = %v, want virtualization gap", err)
		}
		if len(report.Violations) != 1 || report.Violations[0].Target != "GET /repos/acme/web/issues/99" {
			t.Fatalf("violations = %+v", report.Violations)
		}
		if !bytes.Contains(report.Journal, []byte(BenchmarkErrorVirtualizationGap)) {
			t.Fatalf("journal does not report the gap:\n%s", report.Journal)
		}
	})
	t.Run("task provider mismatch", func(t *testing.T) {
		sandbox := newBenchmarkTestSandbox(t, loadBenchmarkTestFixture(t, nil))
		req := benchmarkRunRequest(t.TempDir())
		req.Envelope.RepoRef.Provider = apiv1.ProviderADO
		called := false
		_, err := sandbox.Adapter(func(context.Context, RunRequest, *BenchmarkSandbox) error {
			called = true
			return nil
		}).Run(context.Background(), req)
		if !errors.Is(err, ErrBenchmarkVirtualizationGap) || called {
			t.Fatalf("Run error = %v (act called %v), want gap before the agent runs", err, called)
		}
	})
	t.Run("unvirtualized subprocess", func(t *testing.T) {
		_, _, err := runBenchmark(t, loadBenchmarkTestFixture(t, nil),
			func(ctx context.Context, _ RunRequest, s *BenchmarkSandbox) error {
				_, err := s.RunWithEnv(ctx, []string{"GITHUB_TOKEN=x"}, "gh", "api", "user")
				return err
			})
		if !errors.Is(err, ErrBenchmarkVirtualizationGap) {
			t.Fatalf("Run error = %v, want virtualization gap", err)
		}
	})
}

func TestBenchmarkBoundaryPerformsNoNetworkIO(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	var connections atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			_ = conn.Close()
		}
	}()
	_, _, err = runBenchmark(t, loadBenchmarkTestFixture(t, nil),
		func(ctx context.Context, _ RunRequest, s *BenchmarkSandbox) error {
			provider, err := s.GitHubProvider()
			if err != nil {
				return err
			}
			provider.BaseURL = "http://" + listener.Addr().String()
			_, err = provider.CreateWorkItemComment(ctx, benchmarkTestRepo, "7", "escape")
			return err
		})
	if !errors.Is(err, ErrBenchmarkMutationDenied) {
		t.Fatalf("Run error = %v, want off-boundary request denied", err)
	}
	if n := connections.Load(); n != 0 {
		t.Fatalf("listener accepted %d connections, want none", n)
	}
}

func TestBenchmarkWritesStayInWorkspace(t *testing.T) {
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside.txt")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(parent, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	sandbox := newBenchmarkTestSandbox(t, loadBenchmarkTestFixture(t, nil))
	var escapes []error
	_, err := sandbox.Adapter(func(_ context.Context, _ RunRequest, s *BenchmarkSandbox) error {
		if err := s.WriteFile(filepath.Join("src", "fix.go"), []byte("package src\n")); err != nil {
			return err
		}
		escapes = append(escapes,
			s.WriteFile(filepath.Join("..", "outside.txt"), []byte("clobbered")),
			s.WriteFile(outside, []byte("clobbered")))
		return nil
	}).Run(context.Background(), benchmarkRunRequest(workspace))
	if !errors.Is(err, ErrBenchmarkMutationDenied) {
		t.Fatalf("Run error = %v, want escaping write denied", err)
	}
	for _, escape := range escapes {
		if !errors.Is(escape, ErrBenchmarkMutationDenied) {
			t.Fatalf("escaping write error = %v, want denial", escape)
		}
	}
	if got, _ := os.ReadFile(outside); string(got) != "original" {
		t.Fatalf("outside file = %q, want untouched", got)
	}
	if got, _ := os.ReadFile(filepath.Join(workspace, "src", "fix.go")); string(got) != "package src\n" {
		t.Fatalf("in-workspace write = %q", got)
	}
}

func TestBenchmarkReplaysRecordedFailure(t *testing.T) {
	recorded := &journal.ErrorDetail{Code: "harness_timeout", Message: "agent exceeded its time budget"}
	out, report, err := runBenchmark(t, loadBenchmarkTestFixture(t, recorded), nil)
	var failure *BenchmarkRecordedFailure
	if !errors.As(err, &failure) || failure.Detail.Code != recorded.Code || failure.Detail.Message != recorded.Message {
		t.Fatalf("Run error = %v, want recorded failure", err)
	}
	if out.Payload != nil || report.Status != "failure" || !bytes.Contains(report.Journal, []byte("harness_timeout")) {
		t.Fatalf("payload %s status %q journal:\n%s", out.Payload, report.Status, report.Journal)
	}
}

func TestBenchmarkAdapterRunsThroughExecutor(t *testing.T) {
	sandbox := newBenchmarkTestSandbox(t, loadBenchmarkTestFixture(t, nil))
	var sawCredentials bool
	adapter := sandbox.Adapter(func(ctx context.Context, req RunRequest, s *BenchmarkSandbox) error {
		sawCredentials = req.Credentials != nil
		return triageAct(ctx, req, s)
	})
	rec := &fakeRecorder{}
	exec, err := NewExecutor(adapter, testInjector(t, "", "", noopRegistrar{}), rec, rec, rec, journal.NewPatternScrubber(), "")
	if err != nil {
		t.Fatalf("NewExecutor: %v", err)
	}
	result, err := exec.Invoke(context.Background(), testEnvelope(t.TempDir(), "repo:read"))
	if err != nil || result.Status != apiv1.ResultSuccess || result.Summary != "recorded fix" {
		t.Fatalf("Invoke = %+v, %v", result, err)
	}
	if sawCredentials {
		t.Fatal("benchmark agent received materialized credentials")
	}
	report, err := sandbox.Report()
	if err != nil || len(report.Receipts) != 1 || report.Status != "success" {
		t.Fatalf("report = %+v, %v", report, err)
	}
}

// withGraphQLExchanges appends recorded POST /graphql exchanges to fixture.
func withGraphQLExchanges(fixture BenchmarkFixture, exchanges map[string]string) BenchmarkFixture {
	names := make([]string, 0, len(exchanges))
	for name := range exchanges {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		fixture.Provider.Exchanges = append(fixture.Provider.Exchanges, providerfixture.Exchange{
			Name: name, Method: http.MethodPost, Path: "/graphql",
			Response: providerfixture.FixtureResponse{Status: http.StatusOK, Body: json.RawMessage(exchanges[name])},
		})
	}
	return fixture
}

const (
	benchmarkGraphQLPullRead = `{"data":{"repository":{"pullRequest":{"id":"PR_8","state":"OPEN","merged":false,` +
		`"mergeCommit":null,"mergeQueueEntry":{"state":"QUEUED","position":1},"labels":{"nodes":[]}}}}}`
	benchmarkGraphQLUnqueuedPullRead = `{"data":{"repository":{"pullRequest":{"id":"PR_8","state":"OPEN",` +
		`"merged":false,"mergeCommit":null,"mergeQueueEntry":null}}}}`
	benchmarkGraphQLMergedPullRead = `{"data":{"repository":{"pullRequest":{"id":"PR_8","state":"MERGED","merged":true,` +
		`"mergeCommit":{"oid":"c0ffee"},"mergeQueueEntry":null,"labels":{"nodes":[]}}}}}`
	benchmarkGraphQLResolved = `{"data":{"resolveReviewThread":{"thread":{"id":"RT_1","isResolved":true}}}}`
)

func TestBenchmarkGraphQLMutationsFailClosed(t *testing.T) {
	cases := map[string]struct {
		exchanges map[string]string
		act       func(context.Context, *providers.GitHubProvider) error
		effect    BenchmarkEffect
		target    string
	}{
		"merge through graphql": {
			exchanges: map[string]string{BenchmarkGraphQLQueryExchangePrefix + "EnqueuePullRequestLookup": benchmarkGraphQLUnqueuedPullRead},
			effect:    BenchmarkEffectMerge, target: "POST /graphql mutation enqueuePullRequest",
			act: func(ctx context.Context, p *providers.GitHubProvider) error {
				_, err := p.EnqueuePullRequest(ctx, providers.EnqueuePullRequestRequest{Repository: benchmarkTestRepo, PullID: "8"})
				return err
			},
		},
		"unrelated read or sink cannot satisfy a mutation": {
			exchanges: map[string]string{
				BenchmarkGraphQLQueryExchangePrefix + "PollMergeQueueEntry":   benchmarkGraphQLPullRead,
				BenchmarkGraphQLMutationExchangePrefix + "dequeuePullRequest": benchmarkGraphQLResolved,
			},
			effect: BenchmarkEffectProviderWrite, target: "POST /graphql mutation resolveReviewThread",
			act: func(ctx context.Context, p *providers.GitHubProvider) error {
				return p.ResolvePullRequestReviewThread(ctx, benchmarkTestRepo, "RT_1")
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var actErr error
			_, report, err := runBenchmark(t, withGraphQLExchanges(loadBenchmarkTestFixture(t, nil), tc.exchanges),
				func(ctx context.Context, _ RunRequest, s *BenchmarkSandbox) error {
					provider, err := s.GitHubProvider()
					if err != nil {
						return err
					}
					actErr = tc.act(ctx, provider)
					return nil
				})
			if !errors.Is(actErr, ErrBenchmarkMutationDenied) {
				t.Fatalf("act error = %v, want ErrBenchmarkMutationDenied", actErr)
			}
			var violation *BenchmarkViolation
			if !errors.As(err, &violation) || violation.Effect != tc.effect || violation.Target != tc.target {
				t.Fatalf("Run error = %v (%+v), want %s violation on %q", err, violation, tc.effect, tc.target)
			}
			if len(report.Receipts) != 0 {
				t.Fatalf("receipts = %+v, want no virtual sink used", report.Receipts)
			}
		})
	}
}

func TestBenchmarkGraphQLQueriesAreBoundToTheirOperation(t *testing.T) {
	exchanges := map[string]string{
		BenchmarkGraphQLQueryExchangePrefix + "EnqueuePullRequestLookup": benchmarkGraphQLPullRead,
		BenchmarkGraphQLQueryExchangePrefix + "PollMergeQueueEntry":      benchmarkGraphQLMergedPullRead,
	}
	_, report, err := runBenchmark(t, withGraphQLExchanges(loadBenchmarkTestFixture(t, nil), exchanges),
		func(ctx context.Context, _ RunRequest, s *BenchmarkSandbox) error {
			provider, err := s.GitHubProvider()
			if err != nil {
				return err
			}
			enqueued, err := provider.EnqueuePullRequest(ctx, providers.EnqueuePullRequestRequest{Repository: benchmarkTestRepo, PullID: "8"})
			if err != nil {
				return err
			}
			if enqueued.Merged || !strings.Contains(enqueued.Message, "already enqueued") {
				return fmt.Errorf("enqueue = %+v, want the recorded lookup's queued entry", enqueued)
			}
			for range 2 {
				result, err := provider.PollMergeQueueEntry(ctx, providers.PollMergeQueueEntryRequest{Repository: benchmarkTestRepo, PullID: "8"})
				if err != nil {
					return err
				}
				if result.State != providers.MergeQueueEntryMerged || result.MergeSHA != "c0ffee" {
					return fmt.Errorf("poll = %+v, want the recorded poll's merged state", result)
				}
			}
			return nil
		})
	if err != nil || report.Status != "success" || len(report.Receipts) != 0 {
		t.Fatalf("Run = %v, status %q receipts %+v; want each query served its own recorded read", err, report.Status, report.Receipts)
	}
}

func TestBenchmarkUnmatchedGraphQLQueryIsAGap(t *testing.T) {
	exchanges := map[string]string{
		BenchmarkGraphQLQueryExchangePrefix + "EnqueuePullRequestLookup": benchmarkGraphQLPullRead,
		"unbound-poll": benchmarkGraphQLMergedPullRead,
	}
	_, report, err := runBenchmark(t, withGraphQLExchanges(loadBenchmarkTestFixture(t, nil), exchanges),
		func(ctx context.Context, _ RunRequest, s *BenchmarkSandbox) error {
			provider, err := s.GitHubProvider()
			if err != nil {
				return err
			}
			_, err = provider.PollMergeQueueEntry(ctx, providers.PollMergeQueueEntryRequest{Repository: benchmarkTestRepo, PullID: "8"})
			return err
		})
	if !errors.Is(err, ErrBenchmarkVirtualizationGap) {
		t.Fatalf("Run error = %v, want virtualization gap", err)
	}
	if len(report.Violations) != 1 || report.Violations[0].Target != "POST /graphql query PollMergeQueueEntry" {
		t.Fatalf("violations = %+v, want the unmatched query reported", report.Violations)
	}
}

func TestBenchmarkBoundGraphQLMutationIsVirtualizedOnce(t *testing.T) {
	sink := BenchmarkGraphQLMutationExchangePrefix + "resolveReviewThread"
	var second error
	_, report, err := runBenchmark(t, withGraphQLExchanges(loadBenchmarkTestFixture(t, nil),
		map[string]string{sink: benchmarkGraphQLResolved}),
		func(ctx context.Context, _ RunRequest, s *BenchmarkSandbox) error {
			provider, err := s.GitHubProvider()
			if err != nil {
				return err
			}
			if err := provider.ResolvePullRequestReviewThread(ctx, benchmarkTestRepo, "RT_1"); err != nil {
				return err
			}
			second = provider.ResolvePullRequestReviewThread(ctx, benchmarkTestRepo, "RT_1")
			return nil
		})
	if len(report.Receipts) != 1 || report.Receipts[0].Exchange != sink ||
		report.Receipts[0].Effect != BenchmarkEffectProviderWrite {
		t.Fatalf("receipts = %+v, want one receipt from %s", report.Receipts, sink)
	}
	if !errors.Is(second, ErrBenchmarkMutationDenied) || !errors.Is(err, ErrBenchmarkMutationDenied) {
		t.Fatalf("second mutation = %v, Run = %v; want the consumed sink to deny it", second, err)
	}
}

func benchmarkExchange(method, path, name, body string) providerfixture.Exchange {
	return providerfixture.Exchange{
		Name: name, Method: method, Path: path,
		Response: providerfixture.FixtureResponse{Status: http.StatusOK, Body: json.RawMessage(body)},
	}
}

func reviewThreadsPage(threadID string, commentID int, cursor string) string {
	return fmt.Sprintf(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[{"id":%q,"isResolved":true,`+
		`"path":"login.go","comments":{"nodes":[{"databaseId":%d}]}}],"pageInfo":{"hasNextPage":%t,"endCursor":%q}}}}}}`,
		threadID, commentID, cursor != "", cursor)
}

func reviewThreadsAct(threads *providers.PullRequestReviewThreads) BenchmarkAct {
	return func(ctx context.Context, _ RunRequest, s *BenchmarkSandbox) error {
		provider, err := s.GitHubProvider()
		if err != nil {
			return err
		}
		*threads, err = provider.ListPullRequestReviewThreads(ctx, benchmarkTestRepo, "8")
		return err
	}
}

func reviewThreadsFixture(t *testing.T, pages ...string) BenchmarkFixture {
	t.Helper()
	fixture := loadBenchmarkTestFixture(t, nil)
	fixture.Provider.Exchanges = append(fixture.Provider.Exchanges,
		benchmarkExchange(http.MethodGet, "/repos/acme/web/pulls/8/reviews?per_page=100", "reviews", `[]`),
		benchmarkExchange(http.MethodGet, "/repos/acme/web/pulls/8/comments?per_page=100", "inline-comments",
			`[{"id":101,"body":"one","path":"login.go"},{"id":202,"body":"two","path":"login.go"}]`))
	for _, page := range pages {
		fixture.Provider.Exchanges = append(fixture.Provider.Exchanges, benchmarkExchange(http.MethodPost, "/graphql",
			BenchmarkGraphQLQueryExchangePrefix+"PullRequestReviewThreads", page))
	}
	return fixture
}

func TestBenchmarkPaginatedGraphQLReplayReachesLastPage(t *testing.T) {
	var threads providers.PullRequestReviewThreads
	_, report, err := runBenchmark(t, reviewThreadsFixture(t,
		reviewThreadsPage("RT_1", 101, "CURSOR_1"), reviewThreadsPage("RT_2", 202, "")), reviewThreadsAct(&threads))
	if err != nil || report.Status != "success" {
		t.Fatalf("Run = %v, status %q; want both pages replayed", err, report.Status)
	}
	if len(threads.InlineComments) != 2 || threads.InlineComments[1].ThreadID != "RT_2" {
		t.Fatalf("inline comments = %+v, want comment 202 bound to the page-2 thread", threads.InlineComments)
	}
}

func TestBenchmarkGraphQLReadPastFixtureIsAGap(t *testing.T) {
	var threads providers.PullRequestReviewThreads
	_, report, err := runBenchmark(t, reviewThreadsFixture(t, reviewThreadsPage("RT_1", 101, "CURSOR_1")), reviewThreadsAct(&threads))
	if !errors.Is(err, ErrBenchmarkVirtualizationGap) {
		t.Fatalf("Run error = %v, want the unrecorded page reported as a gap", err)
	}
	if len(report.Violations) != 1 || report.Violations[0].Target != "POST /graphql query PullRequestReviewThreads" {
		t.Fatalf("violations = %+v", report.Violations)
	}
}

func TestBenchmarkGraphQLPollReplaysStateChanges(t *testing.T) {
	fixture := loadBenchmarkTestFixture(t, nil)
	poll := BenchmarkGraphQLQueryExchangePrefix + "PollMergeQueueEntry"
	fixture.Provider.Exchanges = append(fixture.Provider.Exchanges,
		benchmarkExchange(http.MethodPost, "/graphql", poll, benchmarkGraphQLPullRead),
		benchmarkExchange(http.MethodPost, "/graphql", poll, benchmarkGraphQLMergedPullRead))
	var states []providers.MergeQueueEntryState
	_, _, err := runBenchmark(t, fixture, func(ctx context.Context, _ RunRequest, s *BenchmarkSandbox) error {
		provider, err := s.GitHubProvider()
		if err != nil {
			return err
		}
		for range 3 {
			result, err := provider.PollMergeQueueEntry(ctx, providers.PollMergeQueueEntryRequest{Repository: benchmarkTestRepo, PullID: "8"})
			if err != nil {
				return err
			}
			states = append(states, result.State)
		}
		return nil
	})
	want := []providers.MergeQueueEntryState{providers.MergeQueueEntryPending, providers.MergeQueueEntryMerged, providers.MergeQueueEntryMerged}
	if err != nil || !slices.Equal(states, want) {
		t.Fatalf("Run = %v, poll states = %v; want %v", err, states, want)
	}
}

func TestBenchmarkRunawayGraphQLPollFailsClosed(t *testing.T) {
	_, _, err := runBenchmark(t, withGraphQLExchanges(loadBenchmarkTestFixture(t, nil),
		map[string]string{BenchmarkGraphQLQueryExchangePrefix + "PollMergeQueueEntry": benchmarkGraphQLPullRead}),
		func(ctx context.Context, _ RunRequest, s *BenchmarkSandbox) error {
			provider, err := s.GitHubProvider()
			if err != nil {
				return err
			}
			for {
				if _, err := provider.PollMergeQueueEntry(ctx, providers.PollMergeQueueEntryRequest{Repository: benchmarkTestRepo, PullID: "8"}); err != nil {
					return err
				}
			}
		})
	if !errors.Is(err, ErrBenchmarkVirtualizationGap) {
		t.Fatalf("Run error = %v, want a non-converging poll reported as a gap", err)
	}
}

func TestBenchmarkProviderRequestHonorsCancellation(t *testing.T) {
	sandbox := newBenchmarkTestSandbox(t, loadBenchmarkTestFixture(t, nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, BenchmarkProviderBaseURL+"/repos/acme/web/issues/7", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.Do(req); !errors.Is(err, context.Canceled) {
		t.Fatalf("Do error = %v, want context.Canceled", err)
	}
	if report, err := sandbox.Report(); err != nil || len(report.Violations) != 0 {
		t.Fatalf("report = %+v, %v; want cancellation to be no boundary violation", report, err)
	}
}
