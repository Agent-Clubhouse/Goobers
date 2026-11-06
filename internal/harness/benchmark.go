package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
)

const (
	BenchmarkFixtureSchema = "goobers.dev/workflow-gym/fixture/v1"

	BenchmarkErrorVirtualizationGap = "workflow_gym_virtualization_gap"
	BenchmarkErrorMutationDenied    = "workflow_gym_production_mutation_denied"
	BenchmarkErrorFixtureFailure    = "workflow_gym_fixture_failure"
)

var (
	ErrBenchmarkVirtualizationGap = errors.New("workflow gym: virtualization gap")
	ErrBenchmarkMutationDenied    = errors.New("workflow gym: production mutation denied")
)

// BenchmarkOperationKind is one recorded provider interaction or side effect.
type BenchmarkOperationKind string

const (
	BenchmarkProviderRead   BenchmarkOperationKind = "provider-read"
	BenchmarkWorkspaceWrite BenchmarkOperationKind = "workspace-write"
	BenchmarkCredentialRead BenchmarkOperationKind = "credential-read"
	BenchmarkGitPush        BenchmarkOperationKind = "git-push"
	BenchmarkProviderWrite  BenchmarkOperationKind = "provider-write"
	BenchmarkComment        BenchmarkOperationKind = "comment"
	BenchmarkMerge          BenchmarkOperationKind = "merge"
)

// BenchmarkFixture is a recorded, self-contained Workflow Gym harness session.
// Mutating operations are descriptions only: Replay never performs them.
type BenchmarkFixture struct {
	Schema       string                 `json:"schema"`
	Provider     string                 `json:"provider"`
	Transcript   string                 `json:"transcript,omitempty"`
	Artifacts    []BenchmarkArtifact    `json:"artifacts,omitempty"`
	Metrics      map[string]float64     `json:"metrics,omitempty"`
	Operations   []BenchmarkOperation   `json:"operations,omitempty"`
	VirtualSinks []BenchmarkVirtualSink `json:"virtualSinks,omitempty"`
	FailureMode  string                 `json:"failureMode,omitempty"`
}

// BenchmarkArtifact is artifact content captured in a fixture.
type BenchmarkArtifact struct {
	Name      string `json:"name"`
	MediaType string `json:"mediaType,omitempty"`
	Content   string `json:"content"`
}

// BenchmarkOperation is a recorded read or a requested mutation. Response is
// required for provider reads. Sink names a fixture-declared virtual sink for
// every mutating operation.
type BenchmarkOperation struct {
	Kind     BenchmarkOperationKind `json:"kind"`
	Provider string                 `json:"provider,omitempty"`
	Target   string                 `json:"target"`
	Sink     string                 `json:"sink,omitempty"`
	Response json.RawMessage        `json:"response,omitempty"`
}

// BenchmarkVirtualSink authorizes one kind of side effect to be simulated.
// It never grants access to a production implementation.
type BenchmarkVirtualSink struct {
	Name string                 `json:"name"`
	Kind BenchmarkOperationKind `json:"kind"`
}

// BenchmarkRequest binds a fixture replay to the candidate task and workspace.
type BenchmarkRequest struct {
	TaskID    string
	Provider  string
	Workspace string
}

// BenchmarkRecordedArtifact couples fixture bytes to the journal pointer they
// would occupy. Replay does not write the bytes to disk.
type BenchmarkRecordedArtifact struct {
	Name string
	Data []byte
	Ref  journal.Ref
}

// BenchmarkVirtualReceipt proves a requested side effect reached only its
// declared in-memory sink.
type BenchmarkVirtualReceipt struct {
	Sink   string
	Kind   BenchmarkOperationKind
	Target string
}

// BenchmarkReplay is the complete deterministic result of one fixture replay.
type BenchmarkReplay struct {
	Outcome      Outcome
	Artifacts    []BenchmarkRecordedArtifact
	Events       []journal.Event
	Journal      []byte
	Reads        []json.RawMessage
	VirtualSinks []BenchmarkVirtualReceipt
	FailureMode  string
}

// BenchmarkReplayError is a stable, machine-readable replay refusal.
type BenchmarkReplayError struct {
	Code     string
	Provider string
	Kind     BenchmarkOperationKind
	Target   string
	Message  string
	cause    error
}

func (e *BenchmarkReplayError) Error() string { return e.Message }
func (e *BenchmarkReplayError) Unwrap() error { return e.cause }

// DecodeBenchmarkFixture strictly decodes one recorded fixture.
func DecodeBenchmarkFixture(data []byte) (BenchmarkFixture, error) {
	var fixture BenchmarkFixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		return BenchmarkFixture{}, fmt.Errorf("workflow gym: decode fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return BenchmarkFixture{}, errors.New("workflow gym: fixture contains multiple JSON values")
		}
		return BenchmarkFixture{}, fmt.Errorf("workflow gym: decode fixture: %w", err)
	}
	return fixture, nil
}

// BenchmarkHarness replays recorded sessions without subprocess, network,
// credential, provider, git, or filesystem mutation capabilities.
type BenchmarkHarness struct{}

// Replay projects a fixture into the same Outcome, journal event, and artifact
// reference structures used by production runs.
func (BenchmarkHarness) Replay(ctx context.Context, req BenchmarkRequest, fixture BenchmarkFixture) (BenchmarkReplay, error) {
	replay := BenchmarkReplay{
		Outcome: Outcome{
			Transcript:       []byte(fixture.Transcript),
			TranscriptSchema: telemetry.GenAIEventSchema,
			Metrics:          cloneMetrics(fixture.Metrics),
		},
		FailureMode: fixture.FailureMode,
	}
	replay.Events = append(replay.Events,
		benchmarkEvent(journal.EventRunStarted),
		benchmarkEvent(journal.EventStageStarted),
	)
	replay.Events[1].Stage = req.TaskID
	replay.Events[1].Attempt = 1

	fail := func(replayErr *BenchmarkReplayError) (BenchmarkReplay, error) {
		replay.Events = append(replay.Events, benchmarkErrorEvent(req.TaskID, replayErr.Code, replayErr.Message))
		replay.finish(req.TaskID, "failure")
		if err := replay.marshalJournal(); err != nil {
			return BenchmarkReplay{}, err
		}
		return replay, replayErr
	}

	if err := ctx.Err(); err != nil {
		return BenchmarkReplay{}, err
	}
	if fixture.Schema != BenchmarkFixtureSchema {
		return fail(virtualizationGap(req.Provider, "", "", fmt.Sprintf("fixture schema %q is unsupported", fixture.Schema)))
	}
	if req.TaskID == "" {
		return fail(virtualizationGap(req.Provider, "", "", "task ID is required"))
	}
	if req.Provider == "" || fixture.Provider != req.Provider {
		return fail(virtualizationGap(req.Provider, "", "", fmt.Sprintf("fixture provider %q does not match requested provider %q", fixture.Provider, req.Provider)))
	}
	if req.Workspace == "" {
		return fail(virtualizationGap(req.Provider, BenchmarkWorkspaceWrite, "", "workspace is required"))
	}

	for _, operation := range fixture.Operations {
		if err := ctx.Err(); err != nil {
			return BenchmarkReplay{}, err
		}
		provider := operation.Provider
		if provider == "" {
			provider = fixture.Provider
		}
		if provider != req.Provider {
			return fail(virtualizationGap(provider, operation.Kind, operation.Target,
				fmt.Sprintf("operation provider %q does not match requested provider %q", provider, req.Provider)))
		}
		switch operation.Kind {
		case BenchmarkProviderRead:
			if len(operation.Response) == 0 {
				return fail(virtualizationGap(provider, operation.Kind, operation.Target,
					fmt.Sprintf("no recorded response for provider read %q", operation.Target)))
			}
			if !json.Valid(operation.Response) {
				return fail(virtualizationGap(provider, operation.Kind, operation.Target,
					fmt.Sprintf("recorded response for provider read %q is invalid JSON", operation.Target)))
			}
			replay.Reads = append(replay.Reads, slices.Clone(operation.Response))
		case BenchmarkWorkspaceWrite, BenchmarkCredentialRead, BenchmarkGitPush,
			BenchmarkProviderWrite, BenchmarkComment, BenchmarkMerge:
			target, err := benchmarkVirtualTarget(req.Workspace, operation)
			if err != nil {
				return fail(err)
			}
			if !hasBenchmarkSink(fixture.VirtualSinks, operation.Sink, operation.Kind) {
				return fail(mutationDenied(provider, operation.Kind, operation.Target,
					fmt.Sprintf("%s to %q has no declared virtual sink", operation.Kind, operation.Target)))
			}
			replay.VirtualSinks = append(replay.VirtualSinks, BenchmarkVirtualReceipt{
				Sink: operation.Sink, Kind: operation.Kind, Target: target,
			})
			event := benchmarkEvent(journal.EventRunnerAnnotation)
			event.Name = "workflow-gym.virtual-sink"
			event.Runner = map[string]any{"kind": operation.Kind, "sink": operation.Sink, "target": target}
			replay.Events = append(replay.Events, event)
		default:
			return fail(virtualizationGap(provider, operation.Kind, operation.Target,
				fmt.Sprintf("operation kind %q is not virtualized", operation.Kind)))
		}
	}

	for _, artifact := range fixture.Artifacts {
		if artifact.Name == "" {
			return fail(virtualizationGap(req.Provider, "", "", "fixture artifact name is required"))
		}
		for _, recorded := range replay.Artifacts {
			if recorded.Name == artifact.Name {
				return fail(virtualizationGap(req.Provider, "", artifact.Name,
					fmt.Sprintf("fixture artifact %q is duplicated", artifact.Name)))
			}
		}
		data := []byte(artifact.Content)
		ref, err := journal.ArtifactRef(data)
		if err != nil {
			return BenchmarkReplay{}, err
		}
		ref.MediaType = artifact.MediaType
		replay.Artifacts = append(replay.Artifacts, BenchmarkRecordedArtifact{
			Name: artifact.Name, Data: data, Ref: ref,
		})
		event := benchmarkEvent(journal.EventArtifactRecorded)
		event.Stage = req.TaskID
		event.Attempt = 1
		event.Name = artifact.Name
		event.Ref = &replay.Artifacts[len(replay.Artifacts)-1].Ref
		replay.Events = append(replay.Events, event)
	}

	status := "success"
	if fixture.FailureMode != "" {
		status = "failure"
		replay.Events = append(replay.Events, benchmarkErrorEvent(req.TaskID,
			BenchmarkErrorFixtureFailure, fixture.FailureMode))
	}
	replay.finish(req.TaskID, status)
	if err := replay.marshalJournal(); err != nil {
		return BenchmarkReplay{}, err
	}
	return replay, nil
}

func benchmarkVirtualTarget(workspace string, operation BenchmarkOperation) (string, *BenchmarkReplayError) {
	if operation.Kind != BenchmarkWorkspaceWrite {
		return operation.Target, nil
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", mutationDenied("", operation.Kind, operation.Target, fmt.Sprintf("resolve workspace: %v", err))
	}
	target := operation.Target
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return "", mutationDenied("", operation.Kind, operation.Target, fmt.Sprintf("resolve write target: %v", err))
	}
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", mutationDenied("", operation.Kind, operation.Target,
			fmt.Sprintf("workspace write target %q is outside workspace", operation.Target))
	}
	return filepath.ToSlash(relative), nil
}

func hasBenchmarkSink(sinks []BenchmarkVirtualSink, name string, kind BenchmarkOperationKind) bool {
	if name == "" {
		return false
	}
	for _, sink := range sinks {
		if sink.Name == name && sink.Kind == kind {
			return true
		}
	}
	return false
}

func virtualizationGap(provider string, kind BenchmarkOperationKind, target, message string) *BenchmarkReplayError {
	return &BenchmarkReplayError{
		Code: BenchmarkErrorVirtualizationGap, Provider: provider, Kind: kind,
		Target: target, Message: "workflow gym virtualization gap: " + message,
		cause: ErrBenchmarkVirtualizationGap,
	}
}

func mutationDenied(provider string, kind BenchmarkOperationKind, target, message string) *BenchmarkReplayError {
	return &BenchmarkReplayError{
		Code: BenchmarkErrorMutationDenied, Provider: provider, Kind: kind,
		Target: target, Message: "workflow gym production mutation denied: " + message,
		cause: ErrBenchmarkMutationDenied,
	}
}

func benchmarkEvent(eventType journal.EventType) journal.Event {
	return journal.Event{
		Schema: journal.EventSchema,
		Type:   eventType,
		Time:   time.Unix(0, 0).UTC(),
	}
}

func benchmarkErrorEvent(stage, code, message string) journal.Event {
	event := benchmarkEvent(journal.EventError)
	event.Stage = stage
	event.Attempt = 1
	event.Error = &journal.ErrorDetail{Code: code, Message: message}
	return event
}

func (r *BenchmarkReplay) finish(stage, status string) {
	refs := make([]journal.Ref, len(r.Artifacts))
	for i := range r.Artifacts {
		refs[i] = r.Artifacts[i].Ref
	}
	finished := benchmarkEvent(journal.EventStageFinished)
	finished.Stage = stage
	finished.Attempt = 1
	finished.Status = status
	finished.Artifacts = refs
	r.Events = append(r.Events, finished)
	runFinished := benchmarkEvent(journal.EventRunFinished)
	runFinished.Status = status
	runFinished.Disposition = "produced"
	r.Events = append(r.Events, runFinished)
}

func (r *BenchmarkReplay) marshalJournal() error {
	var out bytes.Buffer
	for i := range r.Events {
		r.Events[i].Seq = uint64(i + 1)
		line, err := journal.MarshalEvent(r.Events[i])
		if err != nil {
			return fmt.Errorf("workflow gym: marshal journal event: %w", err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	r.Journal = out.Bytes()
	return nil
}

func cloneMetrics(metrics map[string]float64) map[string]float64 {
	if metrics == nil {
		return nil
	}
	cloned := make(map[string]float64, len(metrics))
	for name, value := range metrics {
		cloned[name] = value
	}
	return cloned
}
