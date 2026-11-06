package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const (
	// BenchmarkProviderBaseURL is the only provider endpoint the benchmark
	// boundary serves. The reserved .invalid TLD never resolves.
	BenchmarkProviderBaseURL = "https://workflow-gym.invalid"
	// BenchmarkVirtualToken is the non-secret placeholder credential that
	// benchmark providers authenticate with. No real credential is resolved.
	BenchmarkVirtualToken = "workflow-gym-virtual-token"

	// BenchmarkErrorVirtualizationGap reports an effect the fixture cannot serve.
	BenchmarkErrorVirtualizationGap = "workflow_gym_virtualization_gap"
	// BenchmarkErrorMutationDenied reports an attempted production mutation.
	BenchmarkErrorMutationDenied = "workflow_gym_production_mutation_denied"
	// BenchmarkErrorSessionFailed reports any other failed benchmark session.
	BenchmarkErrorSessionFailed = "workflow_gym_session_failed"

	benchmarkVirtualSinkAnnotation = "workflow-gym.virtual-sink"
)

var (
	// ErrBenchmarkVirtualizationGap matches every virtualization-gap violation.
	ErrBenchmarkVirtualizationGap = errors.New("workflow gym: virtualization gap")
	// ErrBenchmarkMutationDenied matches every denied production mutation.
	ErrBenchmarkMutationDenied = errors.New("workflow gym: production mutation denied")
)

// BenchmarkEffect classifies one effect attempted through the boundary.
type BenchmarkEffect string

// Effects the benchmark boundary intercepts.
const (
	BenchmarkEffectProviderRead   BenchmarkEffect = "provider-read"
	BenchmarkEffectProviderWrite  BenchmarkEffect = "provider-write"
	BenchmarkEffectComment        BenchmarkEffect = "comment"
	BenchmarkEffectMerge          BenchmarkEffect = "merge"
	BenchmarkEffectCredentialRead BenchmarkEffect = "credential-read"
	BenchmarkEffectGitPush        BenchmarkEffect = "git-push"
	BenchmarkEffectSubprocess     BenchmarkEffect = "subprocess"
	BenchmarkEffectWorkspaceWrite BenchmarkEffect = "workspace-write"
	BenchmarkEffectArtifact       BenchmarkEffect = "artifact"
)

// BenchmarkViolation is a stable, machine-readable boundary refusal. The
// refused effect was never performed.
type BenchmarkViolation struct {
	Code    string
	Effect  BenchmarkEffect
	Target  string
	Message string
	cause   error
}

// Error returns the violation message.
func (v *BenchmarkViolation) Error() string { return v.Message }

// Unwrap returns ErrBenchmarkVirtualizationGap or ErrBenchmarkMutationDenied.
func (v *BenchmarkViolation) Unwrap() error { return v.cause }

func virtualizationGap(effect BenchmarkEffect, target, message string) *BenchmarkViolation {
	return &BenchmarkViolation{
		Code: BenchmarkErrorVirtualizationGap, Effect: effect, Target: target,
		Message: "workflow gym virtualization gap: " + message, cause: ErrBenchmarkVirtualizationGap,
	}
}

func mutationDenied(effect BenchmarkEffect, target, message string) *BenchmarkViolation {
	return &BenchmarkViolation{
		Code: BenchmarkErrorMutationDenied, Effect: effect, Target: target,
		Message: "workflow gym production mutation denied: " + message, cause: ErrBenchmarkMutationDenied,
	}
}

// BenchmarkReceipt proves a mutation reached only its fixture-declared
// virtual sink: the recorded exchange answered it and nothing left the process.
type BenchmarkReceipt struct {
	Effect   BenchmarkEffect
	Exchange string
	Method   string
	Path     string
}

// BenchmarkReport is the deterministic journal projection of one benchmark
// task, using the production journal event structures.
type BenchmarkReport struct {
	Status     string
	Events     []journal.Event
	Journal    []byte
	Receipts   []BenchmarkReceipt
	Violations []BenchmarkViolation
}

// BenchmarkSandbox is the deny-by-default boundary for one benchmark task. It
// implements the provider runtime seams (providers.HTTPClient,
// providers.TokenSource, providers.CommandRunner, providers.MutationRecorder)
// so the calls a benchmark run actually makes are intercepted: recorded reads
// are served, recorded mutations land in memory, and everything else fails
// closed and is reported. It never opens a network connection, resolves a
// credential, or starts a subprocess.
type BenchmarkSandbox struct {
	fixture BenchmarkFixture
	session benchmarkSession

	mu         sync.Mutex
	workspace  string
	stage      string
	consumed   []bool
	events     []journal.Event
	receipts   []BenchmarkReceipt
	violations []*BenchmarkViolation
	failed     bool
}

var (
	_ providers.HTTPClient       = (*BenchmarkSandbox)(nil)
	_ providers.TokenSource      = (*BenchmarkSandbox)(nil)
	_ providers.CommandRunner    = (*BenchmarkSandbox)(nil)
	_ providers.MutationRecorder = (*BenchmarkSandbox)(nil)
)

// NewBenchmarkSandbox validates fixture's recorded session and opens a
// boundary for one benchmark task.
func NewBenchmarkSandbox(fixture BenchmarkFixture) (*BenchmarkSandbox, error) {
	session, err := projectBenchmarkSession(fixture)
	if err != nil {
		return nil, err
	}
	return &BenchmarkSandbox{
		fixture:  fixture,
		session:  session,
		consumed: make([]bool, len(fixture.Provider.Exchanges)),
		events:   []journal.Event{benchmarkEvent(journal.EventRunStarted)},
	}, nil
}

// Adapter returns the benchmark harness Adapter bound to this sandbox.
func (s *BenchmarkSandbox) Adapter(act BenchmarkAct) *BenchmarkAdapter {
	return &BenchmarkAdapter{sandbox: s, act: act}
}

// GitHubProvider returns the production GitHub provider wired entirely
// through this sandbox's seams.
func (s *BenchmarkSandbox) GitHubProvider() (*providers.GitHubProvider, error) {
	if s.fixture.Provider.Provider != string(providers.ProviderGitHub) {
		return nil, s.violate(virtualizationGap(BenchmarkEffectProviderRead, string(providers.ProviderGitHub),
			fmt.Sprintf("fixture records provider %q, not %q", s.fixture.Provider.Provider, providers.ProviderGitHub)))
	}
	provider := providers.NewGitHubProvider(BenchmarkVirtualToken,
		providers.WithHTTPClient(s),
		providers.WithMutationRecorder(s),
		providers.WithMaxTransientRetries(0),
		providers.WithMaxRateLimitRetries(0),
	)
	provider.BaseURL = BenchmarkProviderBaseURL
	provider.Runner = s
	return provider, nil
}

// Do serves provider HTTP requests from recorded exchanges only.
func (s *BenchmarkSandbox) Do(req *http.Request) (*http.Response, error) {
	path := req.URL.RequestURI()
	body, err := readBenchmarkRequestBody(req)
	if err != nil {
		return nil, s.violate(mutationDenied(BenchmarkEffectProviderWrite, req.Method+" "+path,
			"provider request body is unreadable: "+err.Error()))
	}
	op := classifyBenchmarkRequest(req.Method, req.URL.Path, body)
	if req.URL.Scheme+"://"+req.URL.Host != BenchmarkProviderBaseURL {
		return nil, s.violate(gapOrDenial(op.effect, req.Method+" "+req.URL.Redacted()+op.target(),
			"request is outside the benchmark provider boundary"))
	}
	if auth := req.Header.Get("Authorization"); auth != "" && auth != "Bearer "+BenchmarkVirtualToken {
		return nil, s.violate(mutationDenied(BenchmarkEffectCredentialRead, req.Method+" "+path,
			"provider request carries a credential other than the benchmark virtual token"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, exchange := range s.fixture.Provider.Exchanges {
		if exchange.Method != req.Method || exchange.Path != path || !op.servedBy(exchange.Name) {
			continue
		}
		if op.effect != BenchmarkEffectProviderRead {
			if s.consumed[i] {
				continue
			}
			s.consumed[i] = true
			s.recordReceiptLocked(BenchmarkReceipt{Effect: op.effect, Exchange: exchange.Name, Method: req.Method, Path: path})
		}
		return benchmarkResponse(req, exchange.Response.Status, exchange.Response.Headers, exchange.Response.Body), nil
	}
	return nil, s.violateLocked(gapOrDenial(op.effect, req.Method+" "+path+op.target(), "no recorded exchange"))
}

// readBenchmarkRequestBody reads the request body and leaves an identical,
// re-readable copy in place.
func readBenchmarkRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}

func gapOrDenial(effect BenchmarkEffect, target, reason string) *BenchmarkViolation {
	if effect == BenchmarkEffectProviderRead {
		return virtualizationGap(effect, target, fmt.Sprintf("provider read %s: %s", target, reason))
	}
	return mutationDenied(effect, target, fmt.Sprintf("%s %s: %s; the fixture declares no virtual sink", effect, target, reason))
}

// benchmarkOperation is the boundary's classification of one provider request.
type benchmarkOperation struct {
	effect BenchmarkEffect
	// graphQL marks a request to the GitHub GraphQL endpoint, where every
	// operation shares POST /graphql and only the document tells them apart.
	graphQL bool
	// binding identifies a GraphQL operation: a query's operation name (or
	// its single top-level field when unnamed), or a mutation's single
	// top-level field. It is "" when the document identifies none.
	binding string
}

// servedBy reports whether the recorded exchange named name may answer op.
// REST requests are bound by method and path alone. A GraphQL operation is
// served only by an exchange bound to exactly that query or mutation.
func (op benchmarkOperation) servedBy(name string) bool {
	if !op.graphQL {
		return true
	}
	prefix := BenchmarkGraphQLMutationExchangePrefix
	if op.effect == BenchmarkEffectProviderRead {
		prefix = BenchmarkGraphQLQueryExchangePrefix
	}
	binding, bound := strings.CutPrefix(name, prefix)
	return bound && op.binding != "" && binding == op.binding
}

// target qualifies a violation target with the GraphQL operation, if any.
func (op benchmarkOperation) target() string {
	if !op.graphQL {
		return ""
	}
	kind := " mutation"
	if op.effect == BenchmarkEffectProviderRead {
		kind = " query"
	}
	if op.binding == "" {
		return kind
	}
	return kind + " " + op.binding
}

func classifyBenchmarkRequest(method, path string, body []byte) benchmarkOperation {
	if method == http.MethodPost && path == "/graphql" {
		return classifyBenchmarkGraphQL(body)
	}
	switch {
	case method == http.MethodGet || method == http.MethodHead:
		return benchmarkOperation{effect: BenchmarkEffectProviderRead}
	case strings.HasSuffix(path, "/merge"):
		return benchmarkOperation{effect: BenchmarkEffectMerge}
	case strings.HasSuffix(path, "/comments") || strings.Contains(path, "/comments/"):
		return benchmarkOperation{effect: BenchmarkEffectComment}
	case strings.Contains(path, "/git/refs") || strings.Contains(path, "/contents/"):
		return benchmarkOperation{effect: BenchmarkEffectGitPush}
	default:
		return benchmarkOperation{effect: BenchmarkEffectProviderWrite}
	}
}

func benchmarkResponse(req *http.Request, status int, headers map[string]string, body []byte) *http.Response {
	header := make(http.Header, len(headers))
	for name, value := range headers {
		header.Set(name, value)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}
}

// Token denies every credential read: a benchmark never resolves a real
// credential, and no fixture can declare one.
func (s *BenchmarkSandbox) Token(context.Context) (string, error) {
	return "", s.violate(mutationDenied(BenchmarkEffectCredentialRead, "token-source",
		"credential read through the provider token source"))
}

// Run refuses every subprocess; git pushes are reported as denied mutations.
func (s *BenchmarkSandbox) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	return nil, s.refuseCommand(name, args)
}

// RunWithEnv refuses every subprocess without inspecting the environment,
// which may carry credentials.
func (s *BenchmarkSandbox) RunWithEnv(_ context.Context, _ []string, name string, args ...string) ([]byte, error) {
	return nil, s.refuseCommand(name, args)
}

func (s *BenchmarkSandbox) refuseCommand(name string, args []string) error {
	command := filepath.Base(name)
	if strings.TrimSuffix(command, ".exe") == "git" && slices.Contains(args, "push") {
		return s.violate(mutationDenied(BenchmarkEffectGitPush, "git push",
			"git push has no virtual sink in a provider fixture"))
	}
	return s.violate(virtualizationGap(BenchmarkEffectSubprocess, command,
		fmt.Sprintf("subprocess %q is not virtualized", command)))
}

// RecordExternalRef projects a provider's mutation fact into a ref.touched
// journal event. Only mutations served by a recorded exchange reach it.
func (s *BenchmarkSandbox) RecordExternalRef(_ context.Context, ref providers.ExternalRef) {
	event := benchmarkEvent(journal.EventRefTouched)
	event.ExternalRef = &journal.ExternalRef{Provider: string(ref.Provider), Kind: ref.Operation, ID: ref.Ref}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, event)
}

// WriteFile writes data at a workspace-relative path. The task workspace is
// the only real filesystem sink; any path that would escape it is denied.
func (s *BenchmarkSandbox) WriteFile(relPath string, data []byte) error {
	s.mu.Lock()
	workspace := s.workspace
	s.mu.Unlock()
	if workspace == "" {
		return s.violate(virtualizationGap(BenchmarkEffectWorkspaceWrite, relPath, "no benchmark session is active"))
	}
	if !filepath.IsLocal(relPath) {
		return s.violate(mutationDenied(BenchmarkEffectWorkspaceWrite, filepath.ToSlash(relPath),
			"write target is outside the task workspace"))
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return fmt.Errorf("workflow gym: open workspace: %w", err)
	}
	defer func() { _ = root.Close() }()
	if dir := filepath.Dir(relPath); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return s.violate(mutationDenied(BenchmarkEffectWorkspaceWrite, filepath.ToSlash(relPath), err.Error()))
		}
	}
	if err := root.WriteFile(relPath, data, 0o644); err != nil {
		return s.violate(mutationDenied(BenchmarkEffectWorkspaceWrite, filepath.ToSlash(relPath), err.Error()))
	}
	return nil
}

func (s *BenchmarkSandbox) beginStage(req RunRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stage != "" {
		return fmt.Errorf("workflow gym: benchmark sandbox already ran stage %q", s.stage)
	}
	s.stage = req.Envelope.TaskID
	if s.stage == "" {
		s.stage = "benchmark"
	}
	event := benchmarkEvent(journal.EventStageStarted)
	event.Stage, event.Attempt = s.stage, 1
	s.events = append(s.events, event)
	if req.Workspace == "" {
		return s.violateLocked(virtualizationGap(BenchmarkEffectWorkspaceWrite, "", "task workspace is required"))
	}
	if want := s.fixture.Provider.Provider; string(req.Envelope.RepoRef.Provider) != want {
		return s.violateLocked(virtualizationGap(BenchmarkEffectProviderRead, string(req.Envelope.RepoRef.Provider),
			fmt.Sprintf("task provider %q does not match fixture provider %q", req.Envelope.RepoRef.Provider, want)))
	}
	s.workspace = req.Workspace
	return nil
}

func (s *BenchmarkSandbox) finishStage(runErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspace = ""
	refs := make([]journal.Ref, 0, len(s.session.artifacts))
	for _, artifact := range s.session.artifacts {
		event := benchmarkEvent(journal.EventArtifactRecorded)
		event.Stage, event.Attempt, event.Name, event.Integrity = s.stage, 1, artifact.name, artifact.integrity
		ref := artifact.ref
		event.Ref = &ref
		s.events = append(s.events, event)
		refs = append(refs, artifact.ref)
	}
	status := "success"
	var violation *BenchmarkViolation
	if runErr != nil && !errors.As(runErr, &violation) {
		s.events = append(s.events, benchmarkErrorEvent(s.stage, benchmarkErrorDetail(runErr)))
	}
	if runErr != nil || len(s.violations) > 0 {
		status = "failure"
		s.failed = true
	}
	finished := benchmarkEvent(journal.EventStageFinished)
	finished.Stage, finished.Attempt, finished.Status, finished.Artifacts = s.stage, 1, status, refs
	if len(s.session.metrics) > 0 {
		finished.Outputs = make(map[string]any, len(s.session.metrics))
		for name, value := range s.session.metrics {
			finished.Outputs[name] = value
		}
	}
	s.events = append(s.events, finished)
	return runErr
}

func (s *BenchmarkSandbox) violate(v *BenchmarkViolation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.violateLocked(v)
}

func (s *BenchmarkSandbox) violateLocked(v *BenchmarkViolation) error {
	s.violations = append(s.violations, v)
	s.events = append(s.events, benchmarkErrorEvent(s.stage, journal.ErrorDetail{Code: v.Code, Message: v.Message}))
	return v
}

func (s *BenchmarkSandbox) recordReceiptLocked(receipt BenchmarkReceipt) {
	s.receipts = append(s.receipts, receipt)
	event := benchmarkEvent(journal.EventRunnerAnnotation)
	event.Name = benchmarkVirtualSinkAnnotation
	event.Runner = map[string]any{
		"effect": string(receipt.Effect), "exchange": receipt.Exchange,
		"method": receipt.Method, "path": receipt.Path,
	}
	s.events = append(s.events, event)
}

func (s *BenchmarkSandbox) firstViolation() *BenchmarkViolation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.violations) == 0 {
		return nil
	}
	return s.violations[0]
}

// Report returns the deterministic journal projection of everything the
// boundary observed, closed by a run.finished event.
func (s *BenchmarkSandbox) Report() (BenchmarkReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := "success"
	if s.failed || len(s.violations) > 0 || s.stage == "" {
		status = "failure"
	}
	events := slices.Clone(s.events)
	finished := benchmarkEvent(journal.EventRunFinished)
	finished.Status, finished.Disposition = status, "produced"
	events = append(events, finished)
	var out bytes.Buffer
	for i := range events {
		events[i].Seq = uint64(i + 1)
		line, err := journal.MarshalEvent(events[i])
		if err != nil {
			return BenchmarkReport{}, fmt.Errorf("workflow gym: marshal journal event: %w", err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	report := BenchmarkReport{Status: status, Events: events, Journal: out.Bytes(), Receipts: slices.Clone(s.receipts)}
	for _, violation := range s.violations {
		report.Violations = append(report.Violations, *violation)
	}
	return report, nil
}

func benchmarkEvent(eventType journal.EventType) journal.Event {
	return journal.Event{Schema: journal.EventSchema, Type: eventType, Time: time.Unix(0, 0).UTC()}
}

func benchmarkErrorEvent(stage string, detail journal.ErrorDetail) journal.Event {
	event := benchmarkEvent(journal.EventError)
	if stage != "" {
		event.Stage, event.Attempt = stage, 1
	}
	event.Error = &detail
	return event
}
