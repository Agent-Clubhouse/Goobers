package harness

import (
	"context"
	"errors"
	"fmt"
	"maps"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/providerfixture"
)

// Workflow Gym benchmark replay (#8351). A benchmark fixture is not a new
// cassette format: it is a provider contract fixture (providerfixture) for
// every provider interaction plus a recorded stage journal (events.jsonl and
// content-addressed artifacts) for the harness session's transcript,
// completion, artifacts, costs, and failure mode.
const (
	// BenchmarkAdapterName is the registry name of the benchmark-only harness.
	BenchmarkAdapterName = "workflow-gym-benchmark"
	// BenchmarkTranscriptArtifact names the recorded artifact replayed as the
	// session transcript.
	BenchmarkTranscriptArtifact = "transcript.jsonl"
	// BenchmarkCompletionArtifact names the recorded artifact replayed as the
	// session's completion payload.
	BenchmarkCompletionArtifact = "completion.json"
	// BenchmarkGraphQLMutationExchangePrefix prefixes the name of a recorded
	// POST /graphql exchange that is the virtual sink for one GraphQL
	// mutation; the rest of the name is that mutation's top-level field, as
	// in "graphql-mutation:resolveReviewThread".
	BenchmarkGraphQLMutationExchangePrefix = "graphql-mutation:"
	// BenchmarkGraphQLQueryExchangePrefix prefixes the name of a recorded
	// POST /graphql exchange that answers one GraphQL query; the rest of the
	// name is the query's operation name, or its single top-level field when
	// the query is anonymous, as in "graphql-query:PollMergeQueueEntry".
	BenchmarkGraphQLQueryExchangePrefix = "graphql-query:"
)

// BenchmarkFixture is one recorded benchmark task, expressed only in the
// repository's existing recorded structures.
type BenchmarkFixture struct {
	// Provider holds every recorded provider exchange. A REST exchange is
	// bound by method and path: GET and HEAD exchanges are reads served on
	// every request, and any other recorded exchange is the fixture's
	// declaration of a virtual sink for exactly that mutation, consumed once.
	// GitHub GraphQL shares POST /graphql for every operation, so the
	// document decides: a query is served only by exchanges named
	// BenchmarkGraphQLQueryExchangePrefix plus its operation name (or single
	// top-level field when anonymous), in recorded order, so successive pages
	// and changing poll states replay as recorded. Once they are spent, the
	// last one answers only exact repeats of its last request's variables.
	// An unmatched query, a read with new variables past the recording (such
	// as an unrecorded next page), or one exchange answering more than
	// maxBenchmarkReadRepeats reads is a virtualization gap, and a request
	// whose context is done fails with its context error. A mutation is
	// served once, only by an exchange named
	// BenchmarkGraphQLMutationExchangePrefix plus its single top-level field;
	// anything else fails closed.
	Provider providerfixture.Fixture
	// Events is the recorded stage journal in seq order.
	Events []journal.Event
	// Artifacts holds the recorded artifact bytes keyed by journal digest.
	Artifacts map[string][]byte
}

// LoadBenchmarkFixture reads a provider contract fixture and a recorded run
// journal directory without writing or migrating either.
func LoadBenchmarkFixture(providerFixturePath, journalDir string) (BenchmarkFixture, error) {
	provider, err := providerfixture.Read(providerFixturePath)
	if err != nil {
		return BenchmarkFixture{}, fmt.Errorf("workflow gym: read provider fixture: %w", err)
	}
	reader, err := journal.OpenReadOnly(journalDir)
	if err != nil {
		return BenchmarkFixture{}, fmt.Errorf("workflow gym: open recorded journal: %w", err)
	}
	events, err := reader.Events()
	if err != nil {
		return BenchmarkFixture{}, fmt.Errorf("workflow gym: read recorded journal: %w", err)
	}
	artifacts := make(map[string][]byte)
	for _, event := range events {
		if event.Type != journal.EventArtifactRecorded || event.Ref == nil {
			continue
		}
		data, err := reader.ArtifactBytes(*event.Ref)
		if err != nil {
			return BenchmarkFixture{}, fmt.Errorf("workflow gym: read recorded artifact %q: %w", event.Name, err)
		}
		artifacts[event.Ref.Digest] = data
	}
	return BenchmarkFixture{Provider: provider, Events: events, Artifacts: artifacts}, nil
}

type benchmarkArtifact struct {
	name      string
	ref       journal.Ref
	integrity apiv1.Integrity
	data      []byte
}

// benchmarkSession is the replayable projection of a recorded stage journal.
type benchmarkSession struct {
	transcript []byte
	completion []byte
	artifacts  []benchmarkArtifact
	metrics    map[string]float64
	failure    *journal.ErrorDetail
}

func projectBenchmarkSession(fixture BenchmarkFixture) (benchmarkSession, error) {
	var session benchmarkSession
	names := make(map[string]bool)
	for _, event := range fixture.Events {
		switch event.Type {
		case journal.EventArtifactRecorded:
			artifact, err := recordedBenchmarkArtifact(fixture, event)
			if err != nil {
				return benchmarkSession{}, err
			}
			if names[artifact.name] {
				return benchmarkSession{}, virtualizationGap(BenchmarkEffectArtifact, artifact.name,
					fmt.Sprintf("recorded artifact %q is duplicated", artifact.name))
			}
			names[artifact.name] = true
			session.artifacts = append(session.artifacts, artifact)
			switch artifact.name {
			case BenchmarkTranscriptArtifact:
				session.transcript = artifact.data
			case BenchmarkCompletionArtifact:
				session.completion = artifact.data
			}
		case journal.EventStageFinished:
			session.metrics = numericOutputs(event.Outputs)
		case journal.EventError:
			if session.failure == nil && event.Error != nil {
				failure := *event.Error
				session.failure = &failure
			}
		}
	}
	return session, nil
}

func recordedBenchmarkArtifact(fixture BenchmarkFixture, event journal.Event) (benchmarkArtifact, error) {
	if event.Name == "" || event.Ref == nil {
		return benchmarkArtifact{}, virtualizationGap(BenchmarkEffectArtifact, event.Name,
			"recorded artifact event has no name or ref")
	}
	data, ok := fixture.Artifacts[event.Ref.Digest]
	if !ok || journal.Digest(data) != event.Ref.Digest {
		return benchmarkArtifact{}, virtualizationGap(BenchmarkEffectArtifact, event.Name,
			fmt.Sprintf("recorded artifact %q has no matching content", event.Name))
	}
	return benchmarkArtifact{name: event.Name, ref: *event.Ref, integrity: event.Integrity, data: data}, nil
}

func numericOutputs(outputs map[string]any) map[string]float64 {
	var metrics map[string]float64
	for name, value := range outputs {
		number, ok := value.(float64)
		if !ok {
			continue
		}
		if metrics == nil {
			metrics = make(map[string]float64)
		}
		metrics[name] = number
	}
	return metrics
}

// BenchmarkAct scripts the benchmark agent's work. Every provider, credential,
// subprocess, and filesystem effect it needs must go through sandbox.
type BenchmarkAct func(ctx context.Context, req RunRequest, sandbox *BenchmarkSandbox) error

// BenchmarkAdapter is the benchmark-only harness Adapter: it runs no
// subprocess, replays the recorded session, and routes every effect of its
// scripted Act through the sandbox's deny-by-default boundary.
type BenchmarkAdapter struct {
	sandbox *BenchmarkSandbox
	act     BenchmarkAct
}

var _ Adapter = (*BenchmarkAdapter)(nil)

// Name returns BenchmarkAdapterName.
func (a *BenchmarkAdapter) Name() string { return BenchmarkAdapterName }

// Preflight never probes a host CLI or credential: the benchmark harness has
// neither.
func (a *BenchmarkAdapter) Preflight(context.Context) (PreflightInfo, error) {
	return PreflightInfo{Version: BenchmarkAdapterName}, nil
}

// Run replays one recorded harness session. Real credentials materialized
// for the stage are withheld from Act; any boundary violation fails the
// session closed even if Act ignored the error it was given.
func (a *BenchmarkAdapter) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	if err := validateStandardExecution(req); err != nil {
		return Outcome{}, err
	}
	s := a.sandbox
	if err := s.beginStage(req); err != nil {
		return Outcome{}, s.finishStage(err)
	}
	out := Outcome{Transcript: s.session.transcript, Metrics: maps.Clone(s.session.metrics)}
	if a.act != nil {
		actReq := req
		actReq.Credentials = nil
		if err := a.act(ctx, actReq, s); err != nil && s.firstViolation() == nil {
			return out, s.finishStage(err)
		}
	}
	if violation := s.firstViolation(); violation != nil {
		return out, s.finishStage(violation)
	}
	if failure := s.session.failure; failure != nil {
		return out, s.finishStage(&BenchmarkRecordedFailure{Detail: *failure})
	}
	if s.session.completion != nil {
		if err := s.WriteFile(req.CompletionPath, s.session.completion); err != nil {
			return out, s.finishStage(err)
		}
	}
	payload, err := readCompletion(req.Workspace, req.CompletionPath)
	if err != nil {
		return out, s.finishStage(err)
	}
	out.Payload = payload
	return out, s.finishStage(nil)
}

// BenchmarkRecordedFailure is the recorded session's own failure mode,
// replayed verbatim.
type BenchmarkRecordedFailure struct {
	Detail journal.ErrorDetail
}

// Error reports the recorded failure code and message.
func (f *BenchmarkRecordedFailure) Error() string {
	return fmt.Sprintf("workflow gym recorded failure %s: %s", f.Detail.Code, f.Detail.Message)
}

func benchmarkErrorDetail(err error) journal.ErrorDetail {
	var violation *BenchmarkViolation
	if errors.As(err, &violation) {
		return journal.ErrorDetail{Code: violation.Code, Message: violation.Message}
	}
	var recorded *BenchmarkRecordedFailure
	if errors.As(err, &recorded) {
		return recorded.Detail
	}
	// Callers pass only a non-nil session error, so ErrorDetailFor never returns nil.
	return *journal.ErrorDetailFor(BenchmarkErrorSessionFailed, err)
}

// CheckBenchmarkExpectedFailure verifies that the fake harness observed the
// failure mode declared by a Workflow Gym task.
func CheckBenchmarkExpectedFailure(task apiv1.WorkflowGymTaskManifest, runErr error) error {
	if task.ExpectedFailureMode == "" {
		if runErr != nil {
			return fmt.Errorf("workflow gym task %q unexpectedly failed: %w", task.TaskID, runErr)
		}
		return nil
	}
	if runErr == nil {
		return fmt.Errorf("workflow gym task %q expected failure %q but succeeded", task.TaskID, task.ExpectedFailureMode)
	}
	observed := benchmarkErrorDetail(runErr).Code
	if observed != task.ExpectedFailureMode {
		return fmt.Errorf("workflow gym task %q failure mode = %q, want %q", task.TaskID, observed, task.ExpectedFailureMode)
	}
	return nil
}
