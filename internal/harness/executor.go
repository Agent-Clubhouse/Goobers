package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// ErrDeclaredArtifactMissing is returned when a stage declares
// InputArtifactFile but the harness session ends without producing it — the
// stage fails closed rather than silently dropping the declared artifact,
// mirroring internal/executor's InputResultFile contract.
var ErrDeclaredArtifactMissing = errors.New("harness: declared artifact file not produced")

// ErrDeclaredArtifactPathEscape is returned when a stage's declared
// InputArtifactFile escapes the workspace, lexically or via a symlink (#120)
// — an untrusted (possibly prompt-injected) declaration must never let the
// executor lift an arbitrary host file into the journal as if it were the
// stage's own output. The stage fails closed the same way a missing file
// does, not as a hard executor error.
var ErrDeclaredArtifactPathEscape = errors.New("harness: declared artifact file path escapes the workspace")

// Executor is the engine-facing invoke.Goober implementation for agentic
// stages (GBO-051) — checked at compile time so a signature drift is caught
// here, not at the runner's wiring site.
var _ invoke.Goober = (*Executor)(nil)

// SpanRecorder captures a within-stage trace span (GBO-020) — satisfied by
// (*internal/journal.Run).RecordSpan without this package taking on journal's
// full durability/event-log machinery, only its small, stable Ref value type.
type SpanRecorder interface {
	RecordSpan(stage, name string, data []byte) (journal.Ref, error)
}

// ArtifactRecorder persists stage output bytes into the run journal by content
// digest and returns a pointer to them (#73). It mirrors
// internal/executor.ArtifactRecorder and internal/runner.ArtifactRecorder
// exactly (same RecordArtifact method) so *journal.Run satisfies all three
// structurally, with no adapter needed at any call site.
type ArtifactRecorder interface {
	RecordArtifact(name string, data []byte) (journal.Ref, error)
}

// InputArtifactFile is a workspace-relative path a stage may declare in
// InvocationEnvelope.Inputs (#73). If present once the harness session
// completes, that file's bytes are lifted into a content-addressed journal
// artifact and attached to the stage's result/verdict — the agentic analog of
// internal/executor's InputResultFile ("resultFile") convention, but additive:
// the harness's own self-reported result/verdict JSON (via CompletionPath) is
// unaffected either way. A declared-but-missing file fails the stage closed,
// mirroring InputResultFile's contract.
const InputArtifactFile = "artifactFile"

// Executor adapts one Goober's harness Adapter into the engine-facing
// invoke.Goober seam (GBO-051): the engine only ever sees Invoke/Review;
// harness choice, credential materialization, transcript capture, and the
// completion-contract fail-closed check all happen behind this type. One
// Executor is constructed per Goober (Instructions is goober-level, not
// per-invocation) and reused across its stage invocations.
type Executor struct {
	adapter         Adapter
	injector        *credentials.Injector
	recorder        SpanRecorder
	artifacts       ArtifactRecorder
	contextResolver ContextResolver
	scrubber        journal.Scrubber
	validator       *validate.Validator
	instructions    string
	model           string
	harnessOptions  map[string]apiextensionsv1.JSON
	resultPath      string
	verdictPath     string
	timeout         time.Duration
	transcriptLimit int64
}

// Option configures an Executor at construction.
type Option func(*Executor)

// WithResultPath overrides the workspace-relative path a task's result JSON
// must be written to (default DefaultResultPath).
func WithResultPath(path string) Option { return func(e *Executor) { e.resultPath = path } }

// WithVerdictPath overrides the workspace-relative path a reviewer gate's
// verdict JSON must be written to (default DefaultVerdictPath).
func WithVerdictPath(path string) Option { return func(e *Executor) { e.verdictPath = path } }

// WithTimeout bounds every harness session this Executor drives.
func WithTimeout(d time.Duration) Option { return func(e *Executor) { e.timeout = d } }

// WithTranscriptLimit caps the transcript a subprocess-based Adapter retains
// in memory for every harness session this Executor drives (default
// DefaultMaxTranscriptBytes, #245).
func WithTranscriptLimit(n int64) Option { return func(e *Executor) { e.transcriptLimit = n } }

// WithHarnessConfig supplies the goober's adapter-validated model and opaque
// harness options to every session driven by this Executor.
func WithHarnessConfig(model string, options map[string]apiextensionsv1.JSON) Option {
	return func(e *Executor) {
		e.model = model
		if options != nil {
			e.harnessOptions = make(map[string]apiextensionsv1.JSON, len(options))
			for name, value := range options {
				e.harnessOptions[name] = apiextensionsv1.JSON{Raw: append([]byte(nil), value.Raw...)}
			}
		}
	}
}

// NewExecutor builds an Executor for one goober: adapter is the harness to
// drive, injector resolves credentials scoped per invocation's declared
// capabilities, recorder captures the (scrubbed) transcript as a journal
// span, artifacts lifts a stage's declared InputArtifactFile (if any) into a
// content-addressed journal artifact, contextResolver resolves declared
// ContextPointers' in-journal artifacts into the workspace before invocation
// (#121), scrubber redacts transcript/artifact bytes before they are
// recorded, and instructions is the goober's resolved instructions.md body.
func NewExecutor(adapter Adapter, injector *credentials.Injector, recorder SpanRecorder, artifacts ArtifactRecorder, contextResolver ContextResolver, scrubber journal.Scrubber, instructions string, opts ...Option) (*Executor, error) {
	if adapter == nil {
		return nil, fmt.Errorf("harness: executor requires a non-nil adapter")
	}
	if injector == nil {
		return nil, fmt.Errorf("harness: executor requires a non-nil injector")
	}
	if recorder == nil {
		return nil, fmt.Errorf("harness: executor requires a non-nil recorder")
	}
	if artifacts == nil {
		return nil, fmt.Errorf("harness: executor requires a non-nil artifact recorder")
	}
	if contextResolver == nil {
		return nil, fmt.Errorf("harness: executor requires a non-nil context resolver")
	}
	if scrubber == nil {
		return nil, fmt.Errorf("harness: executor requires a non-nil scrubber")
	}
	v, err := validate.New()
	if err != nil {
		return nil, fmt.Errorf("harness: init validator: %w", err)
	}
	e := &Executor{
		adapter:         adapter,
		injector:        injector,
		recorder:        recorder,
		artifacts:       artifacts,
		contextResolver: contextResolver,
		scrubber:        scrubber,
		validator:       v,
		instructions:    instructions,
		resultPath:      DefaultResultPath,
		verdictPath:     DefaultVerdictPath,
	}
	for _, opt := range opts {
		opt(e)
	}
	if e.timeout <= 0 {
		// A caller that never sets WithTimeout must still get a bounded
		// session, not an unbounded one (#119) — DefaultTimeout is applied
		// here, at construction, rather than requiring every call site
		// (cmd/goobers/runnerwiring.go included) to remember to pass it.
		e.timeout = DefaultTimeout
	}
	return e, nil
}

// Invoke implements invoke.Goober: runs the agentic task through the
// configured adapter and returns its result envelope, or an error if the
// stage never produced a valid one (fail closed, GBO-013/GBO-014).
func (e *Executor) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	out, err := e.run(ctx, ModeInvoke, env, e.resultPath)
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if err := e.validator.ValidateEnvelope("result", out.Payload); err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("%w: %w", ErrInvalidCompletion, err)
	}
	var result apiv1.ResultEnvelope
	if err := json.Unmarshal(out.Payload, &result); err != nil {
		return apiv1.ResultEnvelope{}, fmt.Errorf("%w: decode result envelope: %w", ErrInvalidCompletion, err)
	}
	if out.TranscriptTruncated {
		// Mirrors internal/executor.ShellExecutor's stdoutTruncated/
		// stderrTruncated outputs (#245): the recorded span already carries
		// the truncation marker inline, but a scalar output lets a caller
		// notice it without parsing transcript text.
		if result.Outputs == nil {
			result.Outputs = map[string]interface{}{}
		}
		result.Outputs["transcriptTruncated"] = true
		result.Outputs["transcriptDroppedBytes"] = float64(out.TranscriptDroppedBytes)
	}
	ptr, err := e.liftArtifactFile(env)
	if err != nil {
		if code, summary, ok := declaredArtifactFailure(err); ok {
			result.Status = apiv1.ResultFailure
			result.Error = &apiv1.ErrorInfo{Code: code, Message: err.Error(), Retryable: false}
			result.Summary = summary
			return result, nil
		}
		return apiv1.ResultEnvelope{}, err
	}
	if ptr != nil {
		result.Artifacts = append(result.Artifacts, *ptr)
	}
	return result, nil
}

// Review implements invoke.Goober: runs an agentic reviewer gate through the
// configured adapter and returns its verdict, or an error if the gate never
// produced a valid one.
func (e *Executor) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	out, err := e.run(ctx, ModeReview, env, e.verdictPath)
	if err != nil {
		return apiv1.Verdict{}, err
	}
	if err := e.validator.ValidateEnvelope("verdict", out.Payload); err != nil {
		return apiv1.Verdict{}, fmt.Errorf("%w: %w", ErrInvalidCompletion, err)
	}
	var verdict apiv1.Verdict
	if err := json.Unmarshal(out.Payload, &verdict); err != nil {
		return apiv1.Verdict{}, fmt.Errorf("%w: decode verdict: %w", ErrInvalidCompletion, err)
	}
	ptr, err := e.liftArtifactFile(env)
	if err != nil {
		if _, summary, ok := declaredArtifactFailure(err); ok {
			verdict.Decision = apiv1.VerdictFail
			verdict.Summary = fmt.Sprintf("%s: %v", summary, err)
			return verdict, nil
		}
		return apiv1.Verdict{}, err
	}
	if ptr != nil {
		verdict.Evidence = append(verdict.Evidence, *ptr)
	}
	return verdict, nil
}

// declaredArtifactFailure classifies an error from liftArtifactFile as a
// normal, non-executor-fault stage failure (the declared file is missing, or
// its path escapes the workspace lexically or via a symlink — #120) that
// Invoke/Review should surface as ResultFailure/VerdictFail, vs. anything
// else, which callers must propagate as a hard executor error instead.
func declaredArtifactFailure(err error) (code, summary string, ok bool) {
	switch {
	case errors.Is(err, ErrDeclaredArtifactMissing):
		return "missing_declared_artifact", "declared artifact file missing", true
	case errors.Is(err, ErrDeclaredArtifactPathEscape):
		return "declared_artifact_path_escape", "declared artifact file path escapes the workspace", true
	default:
		return "", "", false
	}
}

// run materializes capability-scoped credentials, drives the adapter, and
// records whatever transcript was captured — even on failure, so a runner has
// journaled diagnostics (via the returned error plus the recorded span) beyond
// a bare error string.
func (e *Executor) run(ctx context.Context, mode Mode, env apiv1.InvocationEnvelope, completionPath string) (Outcome, error) {
	creds, err := e.injector.Materialize(ctx, env.Capabilities)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: materialize credentials: %w", err)
	}
	contextPaths, err := e.materializeContext(env)
	if err != nil {
		return Outcome{}, err
	}
	req := RunRequest{
		Mode:               mode,
		Envelope:           env,
		Instructions:       e.instructions,
		Model:              e.model,
		HarnessOptions:     e.harnessOptions,
		Workspace:          env.Workspace,
		CompletionPath:     completionPath,
		TelemetryDir:       telemetry.PrepareStageTelemetryDir(env.Workspace),
		Credentials:        creds,
		ContextPaths:       contextPaths,
		Timeout:            invocationTimeout(env, e.timeout),
		MaxTranscriptBytes: e.transcriptLimit,
	}

	out, runErr := e.adapter.Run(ctx, req)
	if len(out.Transcript) > 0 {
		scrubbed := e.scrubber.Scrub(out.Transcript)
		name := fmt.Sprintf("%s.transcript", e.adapter.Name())
		if _, spanErr := e.recorder.RecordSpan(env.TaskID, name, scrubbed); spanErr != nil && runErr == nil {
			runErr = fmt.Errorf("harness: record span: %w", spanErr)
		}
	}
	if runErr != nil {
		wrapped := fmt.Errorf("harness: %s: %w", e.adapter.Name(), runErr)
		// Tag a session timeout at the invoke seam (#724) so the runner can
		// recognize it and apply a stage's OnTimeout salvage policy without
		// importing this package or matching on error strings — mirroring how
		// worktree-provision transients are marked invoke.InfrastructureFailure.
		if errors.Is(runErr, ErrTimeout) {
			return Outcome{}, invoke.Timeout(wrapped)
		}
		return Outcome{}, wrapped
	}
	if len(out.Payload) == 0 {
		// Defense in depth: an Adapter contract violation (nil error, empty
		// payload) still fails closed rather than surfacing a zero-value
		// result/verdict as a false success.
		return Outcome{}, fmt.Errorf("%w: %s", ErrNoCompletion, completionPath)
	}
	return out, nil
}

func invocationTimeout(env apiv1.InvocationEnvelope, fallback time.Duration) time.Duration {
	if env.Limits.MaxDurationSeconds > 0 {
		return time.Duration(env.Limits.MaxDurationSeconds) * time.Second
	}
	return fallback
}

// liftArtifactFile reads a stage's declared InputArtifactFile (if any) out of
// the workspace and records it as a content-addressed journal artifact (#73).
// It returns (nil, nil) when the stage declares no such file — a pure no-op,
// so stages that never opt in are unaffected. A declared-but-missing file
// returns ErrDeclaredArtifactMissing so Invoke/Review can fail the stage
// closed rather than silently drop it.
func (e *Executor) liftArtifactFile(env apiv1.InvocationEnvelope) (*apiv1.ArtifactPointer, error) {
	path, _ := env.Inputs[InputArtifactFile].(string)
	if path == "" {
		return nil, nil
	}
	full, err := apiv1.ResolveContainedPath(env.Workspace, path)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("%w: %s", ErrDeclaredArtifactMissing, path)
		case errors.Is(err, apiv1.ErrPathEscape), errors.Is(err, apiv1.ErrSymlinkEscape):
			return nil, fmt.Errorf("%w: %s: %w", ErrDeclaredArtifactPathEscape, path, err)
		default:
			return nil, fmt.Errorf("harness: resolve declared artifact file %q: %w", path, err)
		}
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrDeclaredArtifactMissing, path)
		}
		return nil, fmt.Errorf("harness: read declared artifact file %q: %w", path, err)
	}
	scrubbed := e.scrubber.Scrub(data)
	ref, err := e.artifacts.RecordArtifact(env.TaskID+"/"+filepath.Base(path), scrubbed)
	if err != nil {
		return nil, fmt.Errorf("harness: record declared artifact file %q: %w", path, err)
	}
	ptr := refToPointer(ref, mediaTypeFor(path))
	return &ptr, nil
}

// refToPointer converts a journal content-address into its wire equivalent —
// same shape, different package, mirroring internal/executor's refToPointer.
func refToPointer(ref journal.Ref, mediaType string) apiv1.ArtifactPointer {
	return apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, MediaType: mediaType, Size: ref.Size}
}

// mediaTypeFor advisorily categorizes a declared artifact file by extension —
// mirrors internal/executor's mediaTypeFor; the digest, not this, is
// authoritative.
func mediaTypeFor(path string) string {
	if strings.HasSuffix(path, ".json") {
		return "application/json"
	}
	return "application/octet-stream"
}
