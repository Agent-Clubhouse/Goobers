package harness

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/sandbox"
	"github.com/goobers/goobers/internal/telemetry"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Mode selects which completion contract a harness session is driven toward:
// a task producing a result envelope, or a reviewer gate producing a verdict.
// Both drive the same harness through the same invocation envelope
// (ARCHITECTURE.md §5); only the awaited completion artifact differs.
type Mode string

const (
	// ModeInvoke drives an agentic task to a result envelope (GBO-013).
	ModeInvoke Mode = "invoke"
	// ModeReview drives an agentic reviewer gate to a verdict.
	ModeReview Mode = "review"
)

// Default workspace-relative paths a harness is instructed to write its
// completion contract to.
const (
	DefaultResultPath  = ".goobers/result.json"
	DefaultVerdictPath = ".goobers/verdict.json"
)

// ErrNoCompletion is returned when a harness session ends without writing its
// declared completion file — the stage fails closed rather than reporting a
// partial success (GBO-013, GBO-014).
var ErrNoCompletion = errors.New("harness: no completion file written")

// ErrTimeout is returned when a harness session does not finish within its
// declared timeout.
var ErrTimeout = errors.New("harness: session timed out")

// ErrCanceled is returned when a harness session's context is canceled for a
// reason other than its own declared timeout (e.g. a hard-shutdown path
// cancels the ctx a caller passed in, distinct from runCtx's own deadline
// elapsing). Currently unreachable in practice — internal/runner's dispatch
// deliberately uses context.WithoutCancel as its drain contract, so no caller
// today ever cancels the ctx an Adapter/ProcessRunner receives — but kept
// distinct from ErrTimeout so a future hard-shutdown path is never mislabeled
// as a retryable timeout (#122).
var ErrCanceled = errors.New("harness: session canceled")

// ErrInvalidCompletion is returned when a harness's completion file exists but
// fails schema validation — fail closed the same as a missing file (GBO-013).
var ErrInvalidCompletion = errors.New("harness: completion file failed validation")

// RunRequest is everything an Adapter needs to drive one harness session in
// an already-prepared workspace (worktree creation and its BaseRef/branch are
// the runner's concern via internal/worktree, not this package's).
type RunRequest struct {
	// Mode selects the completion contract this session must satisfy.
	Mode Mode
	// Envelope is the invocation envelope (goal, context pointers, declared
	// capabilities, workspace path) the runner built for this stage.
	Envelope apiv1.InvocationEnvelope
	// ExecutionPolicy is the parent-intersected policy enforced for a nested run.
	ExecutionPolicy *apiv1.ChildExecutionPolicy
	// SelectedEnvelopeSections is the optional context view requested by an
	// explicit-context nested policy. ExecutionPolicy remains mandatory and is
	// never filtered by this presentation-only selection.
	SelectedEnvelopeSections map[string]any
	// Instructions is the goober's resolved instructions.md body (persona,
	// scope, done-criteria) — goober-level, not per-invocation.
	Instructions string
	// Model is the optional harness-scoped model selected by the goober.
	Model string
	// HarnessOptions are the goober's opaque, adapter-validated settings.
	HarnessOptions map[string]apiextensionsv1.JSON
	// HarnessConfigResolved reports that Model and HarnessOptions are the
	// effective configuration produced during admission, not raw declarations.
	HarnessConfigResolved bool
	// MCPServers are the goober's external MCP declarations for this invocation.
	MCPServers []apiv1.MCPServer
	// Tools is the goober's default-deny tool allowlist.
	Tools []string
	// GoobersIORegistered reports that this adapter has actually wired the
	// goobers-io MCP server for this run (set by the adapter's own
	// withAutoGoobersIO*-equivalent before the prompt is rendered). The
	// prompt's "## goobers-io tools" section is gated on this, not on
	// eligibility alone (#2774) — an adapter that never registers the server
	// must never instruct the model to call tools that don't exist there.
	GoobersIORegistered bool
	// Workspace is the working directory the harness runs in — normally
	// Envelope.Workspace, threaded explicitly so tests can point it
	// elsewhere.
	Workspace string
	// CompletionPath is the workspace-relative path the harness must write
	// its result/verdict JSON to.
	CompletionPath string
	// TelemetryDir is the writable, stage-scoped directory exposed to harness
	// subprocesses as GOOBERS_TELEMETRY_DIR.
	TelemetryDir string
	// Credentials is pre-scoped to Envelope.Capabilities plus BYO refs declared
	// by MCPServers. Token fails closed for anything outside that set.
	Credentials *credentials.Set
	// ContextPaths maps a resolved ContextPointer's Name to the
	// workspace-relative path its in-journal artifact bytes were
	// materialized to (#121) — populated for Artifact-backed pointers only;
	// External pointers carry no in-journal content to resolve. The prompt
	// renderer uses this to reference actual, readable file content instead
	// of an opaque pointer name.
	ContextPaths map[string]string
	// Timeout bounds the harness session; zero means no timeout.
	Timeout time.Duration
	// MaxTranscriptBytes caps the transcript a subprocess-based Adapter
	// retains in memory; non-positive means DefaultMaxTranscriptBytes (#245).
	MaxTranscriptBytes int64
	// Sandbox, when non-nil, is the platform sandbox a subprocess-based
	// Adapter MUST confine its harness session with (S3/#166): writes are
	// restricted to Workspace plus the runtime roots the adapter declares.
	// Nil (the default, and always the value under a "disabled" posture)
	// leaves the launch path byte-identical to the pre-sandbox behavior.
	Sandbox sandbox.Sandbox
	// HarnessVersion is the CLI version captured by startup preflight, passed
	// through so an adapter can record it alongside the effective invocation
	// in run diagnostics (#2962). Empty when preflight did not report one.
	HarnessVersion string
}

// Outcome is what an Adapter hands back after a harness session ends.
type Outcome struct {
	// Payload is the raw bytes read from CompletionPath — unvalidated; the
	// Executor validates it against the mode's schema.
	Payload []byte
	// Metrics contains adapter-observed numeric measures under canonical
	// telemetry names. An absent measure is omitted; an observed zero is kept.
	Metrics map[string]float64
	// ModelUsage preserves the same measures per model when the harness exposes
	// that dimension. Nil measures are unknown; pointers to zero are observed.
	ModelUsage []telemetry.ModelUsage
	// Transcript is the raw (unredacted) harness transcript, for the caller
	// to scrub and record as a journal span (GBO-020). Bounded at
	// MaxTranscriptBytes — a truncated transcript carries a trailing marker
	// (#245), never a silently cut-off blob.
	Transcript []byte
	// RenderedPrompt is the exact initial prompt sent by an adapter when it
	// differs from the default rendering. The Executor uses it when composing
	// the canonical transcript floor.
	RenderedPrompt []byte
	// TranscriptSchema identifies Transcript's record shape when the adapter
	// already produced structured events. Empty means the Executor must emit
	// the adapter-composed canonical prompt/output floor.
	TranscriptSchema string
	// TranscriptTruncated reports whether Transcript was capped.
	TranscriptTruncated bool
	// TranscriptDroppedBytes is how many transcript bytes were discarded past
	// the cap (0 if TranscriptTruncated is false).
	TranscriptDroppedBytes int64
	// Stderr is the separately captured harness subprocess stderr. It is
	// bounded at MaxTranscriptBytes and recorded as a run artifact on failure.
	Stderr []byte
	// InputInspectionReceipts are tool-owned goobers-io records collected
	// independently of adapter transcript telemetry.
	InputInspectionReceipts []mcpio.InputInspectionReceipt
	// InputInspectionReceiptsCollected reports that goobers-io receipt
	// collection was configured, including when no inspection call occurred.
	InputInspectionReceiptsCollected bool
	// MCPServerFailures lists MCP servers this adapter registered for the
	// session that the harness CLI then reported as not connected at
	// invocation time — either an explicit non-"connected" status or the
	// server missing from the CLI's own connection report entirely. A
	// populated list means every tool those servers provide was silently
	// absent from the agent's session even though the resolved config
	// declared them (#3356): the Executor journals it loudly so a downstream
	// MISSING_REQUIRED_TOOLS-shaped block stops wearing an unrelated
	// costume. Populated by adapters that can observe per-server connection
	// state: claude-code from its stream-json system/init event, copilot-cli
	// from its own run log (#3456 — no transcript equivalent exists there).
	// Nil elsewhere, and nil never asserts that servers WERE available.
	MCPServerFailures []MCPServerFailure
	// AgentEvents are normalized structured events emitted by an adapter. The
	// executor journals them after validation; raw transcripts are not parsed
	// to fabricate nested-agent provenance.
	AgentEvents []journal.Event
	// AgentTelemetryFidelity explicitly reports whether structured nested-agent
	// events are complete, partial, or unavailable.
	AgentTelemetryFidelity string
}

// MCPServerFailure identifies one MCP server that was registered for a
// harness session but reported unusable by the harness CLI at invocation
// time (#3356).
type MCPServerFailure struct {
	// Server is the registered MCP server name (e.g. "goobers-io").
	Server string
	// Status is the harness-reported connection status (e.g. "failed"), or
	// "absent" when the harness's connection report did not mention the
	// registered server at all.
	Status string
}

// PreflightInfo describes the installed harness verified before agentic work
// starts. Version is the harness/CLI version used for invocation provenance.
type PreflightInfo struct {
	Version string
}

// ConfigWarningKind identifies a non-blocking harness configuration finding.
type ConfigWarningKind string

const (
	// ConfigWarningModelFallback reports that an unavailable requested model was
	// replaced by the harness default.
	ConfigWarningModelFallback ConfigWarningKind = "model-fallback"
	// ConfigWarningModelUnverified reports that the harness could not be reached
	// to enumerate models, so the requested model name was accepted without
	// being checked against the live catalogue. This is distinct from
	// ConfigWarningModelFallback: there the harness answered and did not offer
	// the model, here it never answered at all.
	ConfigWarningModelUnverified ConfigWarningKind = "model-unverified"
)

// ConfigWarning is a non-blocking finding produced while resolving harness
// configuration.
type ConfigWarning struct {
	Kind    ConfigWarningKind
	Message string
}

// ConfigResolution is the effective harness configuration after validation.
type ConfigResolution struct {
	Model          string
	HarnessOptions map[string]apiextensionsv1.JSON
	Warnings       []ConfigWarning
}

// Adapter is the harness-adapter seam (GBO-051): the only way an agentic
// stage drives an external coding agent. Implementations are security-critical
// (GBO-041) — an Adapter is trusted to materialize only the credentials its
// RunRequest.Credentials grants and never leak them outside the harness
// session's environment.
type Adapter interface {
	// Name identifies this adapter (e.g. "copilot-cli", "fake") in
	// diagnostics/spans. A Registry may expose it under a distinct goober
	// configuration name.
	Name() string
	// Preflight verifies the harness is installed and signed in and returns
	// its version, or an actionable error if not (GBO-011).
	Preflight(ctx context.Context) (PreflightInfo, error)
	// Run drives one harness session to completion (or a failure) and
	// returns whatever it managed to capture. Run returning a non-nil error
	// means the stage did not complete; Run MUST NOT return a nil error
	// together with an empty/absent Payload — callers rely on
	// ErrNoCompletion for that distinction.
	Run(ctx context.Context, req RunRequest) (Outcome, error)
}

// ConfigValidator is implemented by adapters that expose model or option
// configuration. Validation runs while definitions are admitted, before a
// harness process or run attempt starts.
type ConfigValidator interface {
	ValidateConfig(model string, options map[string]apiextensionsv1.JSON) error
}

// ConfigResolver is implemented by adapters whose effective configuration can
// differ from the declaration after an explicit fallback policy is applied.
type ConfigResolver interface {
	ResolveConfig(model string, options map[string]apiextensionsv1.JSON) (ConfigResolution, error)
}

// NestedPolicyCapability declares that an adapter has a distinct child launch
// path which understands and enforces the portable nested-agent policy
// envelope. Validation alone is not sufficient: RunNested must be the method
// that launches the child so the ordinary adapter path cannot silently ignore
// the effective policy.
type NestedPolicyCapability interface {
	ValidateNestedAgentPolicy(apiv1.NestedAgentPolicy) error
	RunNested(context.Context, RunRequest) (Outcome, error)
}

func validateStandardExecution(req RunRequest) error {
	if req.Envelope.NestedAgentPolicy != nil || req.ExecutionPolicy != nil {
		return fmt.Errorf("harness: nested execution requires the adapter child-launch path")
	}
	return nil
}

func validateNestedExecution(req RunRequest) error {
	if req.Envelope.NestedAgentPolicy == nil {
		return fmt.Errorf("harness: nested execution has no policy declaration")
	}
	if req.ExecutionPolicy == nil {
		return fmt.Errorf("harness: nested execution has no admitted child policy")
	}
	if req.ExecutionPolicy.Profile == "" ||
		!slices.Contains(req.Envelope.NestedAgentPolicy.PermittedProfiles, req.ExecutionPolicy.Profile) {
		return fmt.Errorf("harness: nested execution profile was not admitted")
	}
	if !sameStrings(req.ExecutionPolicy.Capabilities, req.Envelope.Capabilities) ||
		!sameStrings(req.ExecutionPolicy.PolicyActions, req.Envelope.PolicyActions) {
		return fmt.Errorf("harness: nested envelope was not narrowed to effective authority")
	}
	if req.Envelope.ParentPlatformPolicy != nil {
		return fmt.Errorf("harness: nested envelope exposes parent platform authority")
	}
	declaration := req.Envelope.NestedAgentPolicy
	if declaration.Delegation != req.ExecutionPolicy.Delegation ||
		declaration.MaxDepth != req.ExecutionPolicy.MaxDepth ||
		declaration.PeerMessaging != req.ExecutionPolicy.PeerMessaging ||
		!sameStrings(declaration.PermittedProfiles, []string{req.ExecutionPolicy.Profile}) ||
		declaration.Context.Mode != req.ExecutionPolicy.Context.Mode ||
		!sameStrings(declaration.Context.ArtifactNames, req.ExecutionPolicy.Context.ArtifactNames) ||
		!sameStrings(declaration.Context.EnvelopeSections, req.ExecutionPolicy.Context.EnvelopeSections) ||
		!sameStrings(declaration.Model.Allowlist, req.ExecutionPolicy.Model.Allowlist) ||
		declaration.Model.MaxReasoningEffort != req.ExecutionPolicy.Model.MaxReasoningEffort ||
		!samePlatformPolicy(declaration.PlatformPolicy, req.ExecutionPolicy.PlatformPolicy) {
		return fmt.Errorf("harness: nested declaration was not reduced to effective authority")
	}
	if !sameStrings(req.ExecutionPolicy.PlatformPolicy.Capabilities, req.ExecutionPolicy.Capabilities) ||
		!sameStrings(req.ExecutionPolicy.PlatformPolicy.PolicyActions, req.ExecutionPolicy.PolicyActions) ||
		req.Envelope.Limits != req.ExecutionPolicy.PlatformPolicy.Budget {
		return fmt.Errorf("harness: nested runtime resources differ from effective authority")
	}
	if len(req.ExecutionPolicy.Model.Allowlist) > 0 &&
		!slices.Contains(req.ExecutionPolicy.Model.Allowlist, req.Model) {
		return fmt.Errorf("harness: nested model was not admitted")
	}
	effort, err := configuredReasoningEffort(req.HarnessOptions)
	if err != nil {
		return err
	}
	if effort != "" &&
		req.ExecutionPolicy.Model.MaxReasoningEffort != "" &&
		nestedReasoningRank(effort) > nestedReasoningRank(req.ExecutionPolicy.Model.MaxReasoningEffort) {
		return fmt.Errorf("harness: nested reasoning effort exceeds effective authority")
	}
	return nil
}

func sameStrings(left, right []string) bool {
	return slices.Equal(left, right)
}

func samePlatformPolicy(left, right apiv1.PlatformPolicy) bool {
	return sameStrings(left.Capabilities, right.Capabilities) &&
		sameStrings(left.PolicyActions, right.PolicyActions) &&
		sameStrings(left.Credentials, right.Credentials) &&
		left.Sandbox == right.Sandbox &&
		sameStrings(left.FilesystemRoots, right.FilesystemRoots) &&
		sameStrings(left.NetworkEgress, right.NetworkEgress) &&
		sameStrings(left.ContentExclusions, right.ContentExclusions) &&
		left.Budget == right.Budget &&
		left.Cancellation == right.Cancellation &&
		left.CompletionContract == right.CompletionContract
}

func nestedReasoningRank(value apiv1.ReasoningEffort) int {
	switch value {
	case apiv1.ReasoningMinimal:
		return 1
	case apiv1.ReasoningLow:
		return 2
	case apiv1.ReasoningMedium:
		return 3
	case apiv1.ReasoningHigh:
		return 4
	default:
		return 0
	}
}

// ValidateNestedAgentPolicy performs the adapter-side admission check and
// returns the adapter's mandatory child-launch path.
func ValidateNestedAgentPolicy(adapter Adapter, policy apiv1.NestedAgentPolicy) (NestedPolicyCapability, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	capability, ok := adapter.(NestedPolicyCapability)
	if !ok {
		return nil, fmt.Errorf("harness: %s does not support nested-agent policy enforcement", adapter.Name())
	}
	if err := capability.ValidateNestedAgentPolicy(policy); err != nil {
		return nil, err
	}
	return capability, nil
}

// ValidateConfig delegates model and option validation to adapter. An adapter
// without a validator may only be used with an empty harness configuration.
func ValidateConfig(adapter Adapter, model string, options map[string]apiextensionsv1.JSON) error {
	validator, ok := adapter.(ConfigValidator)
	if !ok {
		if model != "" || len(options) > 0 {
			return fmt.Errorf("harness: %s does not support model or harness options", adapter.Name())
		}
		return nil
	}
	return validator.ValidateConfig(model, options)
}

// ResolveConfig validates adapter-owned configuration and applies any explicit
// fallback policy exposed by that adapter.
func ResolveConfig(adapter Adapter, model string, options map[string]apiextensionsv1.JSON) (ConfigResolution, error) {
	if resolver, ok := adapter.(ConfigResolver); ok {
		return resolver.ResolveConfig(model, options)
	}
	if err := ValidateConfig(adapter, model, options); err != nil {
		return ConfigResolution{}, err
	}
	return ConfigResolution{
		Model:          model,
		HarnessOptions: cloneHarnessOptions(options),
	}, nil
}

func cloneHarnessOptions(options map[string]apiextensionsv1.JSON) map[string]apiextensionsv1.JSON {
	if options == nil {
		return nil
	}
	cloned := make(map[string]apiextensionsv1.JSON, len(options))
	for name, value := range options {
		cloned[name] = apiextensionsv1.JSON{Raw: append([]byte(nil), value.Raw...)}
	}
	return cloned
}

// readCompletion reads the harness's declared completion file back out of the
// workspace. A missing file is reported as ErrNoCompletion so callers can
// distinguish "the harness never finished" from other I/O failures — the
// fail-closed path every Adapter shares (GBO-013, GBO-014).
func readCompletion(workspace, relPath string) ([]byte, error) {
	full := filepath.Join(workspace, relPath)
	b, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNoCompletion, relPath)
		}
		return nil, fmt.Errorf("harness: read completion file %s: %w", relPath, err)
	}
	return b, nil
}
