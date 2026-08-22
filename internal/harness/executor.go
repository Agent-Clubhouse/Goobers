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
	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sandbox"
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

// SpanRecorder captures a schema-aware within-stage trace span (GBO-020) —
// satisfied by (*internal/journal.Run).RecordSpanWithSchema without this
// package taking on journal's full durability/event-log machinery, only its
// small, stable Ref value type.
type SpanRecorder interface {
	RecordSpanWithSchema(stage, name, dataSchema string, data []byte) (journal.Ref, error)
}

// EventAppender is the optional journal seam an enforced-sandbox Executor
// requires of its recorder — satisfied by (*internal/journal.Run).Append. It
// records the runner.isolation.posture annotation for every enforced agentic
// stage attempt (#1305); a recorder that cannot journal the posture fails the
// enforced stage closed rather than running with an unauditable posture.
type EventAppender interface {
	Append(ev journal.Event) error
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
	assets          *gooberassets.Bundle
	model           string
	harnessVersion  string
	harnessOptions  map[string]apiextensionsv1.JSON
	mcpServers      []apiv1.MCPServer
	tools           []string
	resultPath      string
	verdictPath     string
	timeout         time.Duration
	transcriptLimit int64
	sandboxEnforced bool
	newSandbox      func() (sandbox.Sandbox, error)
}

// Option configures an Executor at construction.
type Option func(*Executor)

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

// WithMCPServers supplies the goober's per-invocation external MCP servers.
func WithMCPServers(servers []apiv1.MCPServer) Option {
	return func(e *Executor) {
		e.mcpServers = copyMCPServers(servers)
	}
}

// WithTools supplies the goober's default-deny tool allowlist.
func WithTools(tools []string) Option {
	return func(e *Executor) {
		e.tools = append([]string(nil), tools...)
	}
}

// WithHarnessVersion supplies the version captured by startup preflight.
func WithHarnessVersion(version string) Option {
	return func(e *Executor) { e.harnessVersion = version }
}

// WithSandboxEnforcement requires every harness session this Executor drives
// to run confined by the platform filesystem sandbox (S3/#166, #1305). The
// composition root sets it when the stage's gaggle resolves to an "enforced"
// isolation posture (instance.EffectiveAgenticSandbox); the default —
// posture "disabled" — leaves the Executor, adapter, and subprocess launch
// byte-identical to the pre-sandbox behavior. Under enforcement each attempt
// journals a runner.isolation.posture annotation, and a host without a usable
// sandbox fails the stage closed with ErrSandboxUnavailable rather than
// running the harness unconfined.
func WithSandboxEnforcement() Option {
	return func(e *Executor) { e.sandboxEnforced = true }
}

// WithAssetBundle supplies the goober's optional static assets.
func WithAssetBundle(bundle *gooberassets.Bundle) Option {
	return func(e *Executor) { e.assets = bundle }
}

// HasAssetBundle reports whether each invocation materializes the reserved
// goober-assets workspace path.
func (e *Executor) HasAssetBundle() bool {
	return e.assets != nil
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
		newSandbox:      sandbox.New,
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
	out, transcript, stderr, err := e.run(ctx, ModeInvoke, env, e.resultPath)
	if err != nil {
		return adapterDiagnostics(out, transcript, stderr), err
	}
	if err := e.validator.ValidateEnvelope("result", out.Payload); err != nil {
		return adapterDiagnostics(out, transcript, stderr), fmt.Errorf("%w: %w", ErrInvalidCompletion, err)
	}
	var result apiv1.ResultEnvelope
	if err := json.Unmarshal(out.Payload, &result); err != nil {
		return adapterDiagnostics(out, transcript, stderr), fmt.Errorf("%w: decode result envelope: %w", ErrInvalidCompletion, err)
	}
	mergeAdapterMetrics(&result, out.Metrics)
	// #2962: settle what actually went wrong before anything downstream acts
	// on the model's own classification. A generic tool-permission refusal is
	// a harness fault the operator can fix; organization content exclusion is
	// a policy fact. Conflated, the former parked driving issues for humans
	// that no human action could unstick.
	reclassifyToolPermissionBlock(&result, out.Transcript, out.Stderr)
	// The transcript pointer is runner-authored. Never trust a harness to
	// self-report a path or digest for the diagnostic bytes the runner captured.
	result.Transcript = transcript
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
		return result, err
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
	out, _, _, err := e.run(ctx, ModeReview, env, e.verdictPath)
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
func (e *Executor) run(ctx context.Context, mode Mode, env apiv1.InvocationEnvelope, completionPath string) (Outcome, *apiv1.ArtifactPointer, *apiv1.ArtifactPointer, error) {
	if env.NestedAgentPolicy != nil {
		if err := ValidateNestedAgentPolicy(e.adapter, *env.NestedAgentPolicy); err != nil {
			return Outcome{}, nil, nil, fmt.Errorf("harness: admit nested-agent policy: %w", err)
		}
		if !nestedCapabilitiesSubset(env.NestedAgentPolicy.PlatformPolicy.Capabilities, env.Capabilities) {
			return Outcome{}, nil, nil, fmt.Errorf("harness: nested-agent capabilities exceed stage authority")
		}
	}
	telemetry.RecordAgentProvenance(ctx, e.model, e.harnessVersion)
	if err := e.assets.Materialize(env.Workspace); err != nil {
		return Outcome{}, nil, nil, fmt.Errorf("harness: materialize goober assets: %w", err)
	}
	creds, err := e.injector.Materialize(ctx, env.Capabilities)
	if err != nil {
		// A credential-materialization failure is an infrastructure fault at
		// stage-environment build time, not evidence about the work (#3361):
		// typed with its own code (executor.StageFailure, so telemetry rows
		// carry credential_unavailable/infra instead of executor_error/
		// unknown) AND marked via the invoke.InfrastructureFailure seam, so
		// the runner retries on the bounded infrastructure budget (journal
		// AttemptClass "infra") instead of consuming the stage's policy
		// attempts — at attempt budgets of 1, the old classification turned a
		// transient provider 403 into a terminal work failure.
		return Outcome{}, nil, nil, invoke.InfrastructureFailure(executor.StageFailure(
			telemetry.ErrCodeCredentialUnavailable,
			fmt.Errorf("harness: materialize credentials: %w", err),
		))
	}
	contextPaths, err := e.materializeContext(env)
	if err != nil {
		return Outcome{}, nil, nil, err
	}
	req := RunRequest{
		Mode:                  mode,
		Envelope:              env,
		Instructions:          e.instructions,
		Model:                 e.model,
		HarnessOptions:        e.harnessOptions,
		HarnessConfigResolved: true,
		MCPServers:            copyMCPServers(e.mcpServers),
		Tools:                 append([]string(nil), e.tools...),
		Workspace:             env.Workspace,
		CompletionPath:        completionPath,
		TelemetryDir:          telemetry.PrepareStageTelemetryDir(env.Workspace),
		Credentials:           creds,
		ContextPaths:          contextPaths,
		Timeout:               invocationTimeout(env, e.timeout),
		MaxTranscriptBytes:    e.transcriptLimit,
		HarnessVersion:        e.harnessVersion,
	}
	if e.sandboxEnforced {
		// Fail closed BEFORE any harness subprocess can start: an enforced
		// posture with no usable platform sandbox must block the stage, never
		// downgrade to an unconfined run (S3/#166, ADR-0001).
		sb, err := e.newSandbox()
		if err != nil {
			return Outcome{}, nil, nil, fmt.Errorf(
				"%w: %w — the effective agentic sandbox posture for this gaggle is %q; install the platform sandbox (macOS: sandbox-exec, Linux: bubblewrap) or set sandbox.agentic to %q in instance.yaml / the gaggle's sandbox override",
				ErrSandboxUnavailable, err, "enforced", "disabled")
		}
		req.Sandbox = sb
		// Journal the posture for this attempt (#1305) — runner.* payload,
		// conformance-excluded. Only enforced attempts emit; a disabled
		// posture writes nothing new anywhere. The audit record is part of
		// the enforcement contract, so a recorder that cannot append it
		// fails the stage closed too.
		appender, ok := e.recorder.(EventAppender)
		if !ok {
			return Outcome{}, nil, nil, fmt.Errorf("harness: sandbox enforcement requires a journal-backed recorder to record the isolation posture; %T cannot append events", e.recorder)
		}
		if err := appender.Append(journal.Event{
			Type:  journal.EventRunnerIsolationPosture,
			Stage: env.TaskID,
			Runner: map[string]any{
				"posture":   "enforced",
				"mechanism": sb.Mechanism(),
				"workspace": env.Workspace,
			},
		}); err != nil {
			return Outcome{}, nil, nil, fmt.Errorf("harness: journal isolation posture for %q: %w", env.TaskID, err)
		}
	}

	out, runErr := e.adapter.Run(ctx, req)
	telemetry.RecordAgentUsage(ctx, out.Metrics, out.ModelUsage)
	invoke.ReportAgentUsage(ctx, out.Metrics)
	if out.InputInspectionReceiptsCollected {
		appender, ok := e.recorder.(EventAppender)
		if !ok {
			runErr = errors.Join(runErr, fmt.Errorf(
				"harness: goobers-io receipt collection requires a journal-backed recorder; %T cannot append events",
				e.recorder,
			))
		} else if err := appender.Append(journal.Event{
			Type:  journal.EventRunnerAnnotation,
			Stage: env.TaskID,
			Runner: map[string]any{
				"kind":     "goobers-io-input-inspection-receipts",
				"receipts": out.InputInspectionReceipts,
				// This annotation is only emitted when collection was
				// configured, so `receipts: null` already means "the agent
				// made no inspection call" rather than "collection was off".
				// That distinction was implicit in the emission rule and
				// invisible to anyone reading the journal: on 2026-08-22 a
				// null here was read as a lost MCP toolset by three separate
				// readers, and disproving it took hand-reading the harness
				// CLI's own log inside the pod. State the count outright so
				// the journal answers it without that knowledge.
				"inspectionCalls": len(out.InputInspectionReceipts),
			},
		}); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf(
				"harness: journal goobers-io input inspection receipts for %q: %w",
				env.TaskID,
				err,
			))
		}
	}
	if len(out.MCPServerFailures) > 0 {
		// A registered MCP server the harness reported as not connected
		// (#3356): every tool it provides was absent from the agent's
		// session even though the resolved config declared it. Journal it
		// loudly next to whatever the stage goes on to report, so a
		// tool-shaped failure (e.g. an agent-authored MISSING_REQUIRED_TOOLS
		// block) names its actual cause instead of surfacing two layers away
		// wearing an unrelated costume. Annotation only — the run's own
		// outcome is untouched, so nothing that worked before changes.
		servers := make([]map[string]string, 0, len(out.MCPServerFailures))
		for _, failure := range out.MCPServerFailures {
			servers = append(servers, map[string]string{
				"server": failure.Server,
				"status": failure.Status,
			})
		}
		if appender, ok := e.recorder.(EventAppender); ok {
			if err := appender.Append(journal.Event{
				Type:  journal.EventRunnerAnnotation,
				Stage: env.TaskID,
				Runner: map[string]any{
					"kind":    "mcp-server-unavailable",
					"servers": servers,
					"detail": "registered MCP servers were not connected at invocation; " +
						"their tools were unavailable to the agent for this whole session — " +
						"any missing-tool failure this stage reports is caused here",
				},
			}); err != nil {
				runErr = errors.Join(runErr, fmt.Errorf(
					"harness: journal MCP server availability for %q: %w",
					env.TaskID,
					err,
				))
			}
		}
	}
	if out.TranscriptSchema == "" {
		prompt := out.RenderedPrompt
		if len(prompt) == 0 {
			prompt = []byte(renderPrompt(req))
		}
		prompt = e.scrubber.Scrub(prompt)
		output := e.scrubber.Scrub(out.Transcript)
		out.Transcript, err = composedTranscript(string(prompt), output, req.Model, out.TranscriptTruncated)
		if err != nil {
			return out, nil, nil, fmt.Errorf("harness: encode transcript floor: %w", err)
		}
		var dropped int64
		out.Transcript, dropped, err = boundCanonicalTranscript(out.Transcript, req.MaxTranscriptBytes, out.TranscriptDroppedBytes)
		if err != nil {
			return out, nil, nil, fmt.Errorf("harness: bound transcript floor: %w", err)
		}
		if dropped > out.TranscriptDroppedBytes {
			out.TranscriptTruncated = true
		}
		out.TranscriptDroppedBytes = dropped
		out.TranscriptSchema = telemetry.GenAIEventSchema
	}
	var transcript *apiv1.ArtifactPointer
	if len(out.Transcript) > 0 {
		scrubbed := e.scrubber.Scrub(out.Transcript)
		name := fmt.Sprintf("%s.transcript", e.adapter.Name())
		ref, spanErr := e.recorder.RecordSpanWithSchema(env.TaskID, name, out.TranscriptSchema, scrubbed)
		if spanErr != nil {
			if runErr == nil {
				runErr = fmt.Errorf("harness: record span: %w", spanErr)
			}
		} else {
			ptr := refToPointer(ref, "")
			transcript = &ptr
		}
	}
	if runErr != nil {
		var stderr *apiv1.ArtifactPointer
		ref, artifactErr := e.artifacts.RecordArtifact(env.TaskID+"/stderr.log", e.scrubber.Scrub(out.Stderr))
		if artifactErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("harness: record stderr: %w", artifactErr))
		} else {
			ptr := refToPointer(ref, "text/plain")
			stderr = &ptr
		}
		wrapped := fmt.Errorf("harness: %s: %w", e.adapter.Name(), runErr)
		// Tag a session timeout at the invoke seam (#724) so the runner can
		// recognize it and apply a stage's OnTimeout salvage policy without
		// importing this package or matching on error strings — mirroring how
		// worktree-provision transients are marked invoke.InfrastructureFailure.
		if errors.Is(runErr, ErrTimeout) {
			return out, transcript, stderr, invoke.Timeout(wrapped)
		}
		if errors.Is(runErr, ErrNoCompletion) {
			return out, transcript, stderr, invoke.InfrastructureFailure(wrapped)
		}
		return out, transcript, stderr, wrapped
	}
	if len(out.Payload) == 0 {
		// Defense in depth: an Adapter contract violation (nil error, empty
		// payload) still fails closed rather than surfacing a zero-value
		// result/verdict as a false success.
		err := fmt.Errorf("%w: %s", ErrNoCompletion, completionPath)
		return out, transcript, nil, invoke.InfrastructureFailure(err)
	}
	return out, transcript, nil, nil
}

func copyMCPServers(servers []apiv1.MCPServer) []apiv1.MCPServer {
	if len(servers) == 0 {
		return nil
	}
	out := make([]apiv1.MCPServer, len(servers))
	for i := range servers {
		out[i] = servers[i]
		out[i].Args = append([]string(nil), servers[i].Args...)
		out[i].CredentialRefs = append([]apiv1.MCPCredentialRef(nil), servers[i].CredentialRefs...)
	}
	return out
}

func mergeAdapterMetrics(result *apiv1.ResultEnvelope, metrics map[string]float64) {
	for name := range result.Metrics {
		if telemetry.IsCanonicalAgentUsageMetric(name) {
			delete(result.Metrics, name)
		}
	}
	if len(result.Metrics) == 0 {
		result.Metrics = nil
	}
	if len(metrics) == 0 {
		return
	}
	if result.Metrics == nil {
		result.Metrics = make(map[string]float64, len(metrics))
	}
	for name, value := range metrics {
		result.Metrics[name] = value
	}
}

func copyMetrics(metrics map[string]float64) map[string]float64 {
	if len(metrics) == 0 {
		return nil
	}
	copied := make(map[string]float64, len(metrics))
	for name, value := range metrics {
		copied[name] = value
	}
	return copied
}

func adapterDiagnostics(out Outcome, transcript, stderr *apiv1.ArtifactPointer) apiv1.ResultEnvelope {
	result := apiv1.ResultEnvelope{
		Transcript: transcript,
		Metrics:    copyMetrics(out.Metrics),
	}
	if stderr != nil {
		result.Artifacts = []apiv1.ArtifactPointer{*stderr}
	}
	return result
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
	return apiv1.ArtifactPointer{
		Path: ref.Path, Digest: ref.Digest, MediaType: mediaType, Size: ref.Size, Integrity: ref.Integrity,
	}
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

func nestedCapabilitiesSubset(values, authority []string) bool {
	for _, value := range values {
		found := false
		for _, allowed := range authority {
			if value == allowed {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
