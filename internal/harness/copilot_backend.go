package harness

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/goobers/goobers/internal/capability"
)

// copilotBackend separates the logical Copilot harness from its execution
// transport. Production remains on copilotCLIBackend until SDK routing is
// explicitly implemented and qualified.
type copilotBackend interface {
	Run(context.Context, RunRequest) (Outcome, error)
}

type copilotCLIBackend struct {
	adapter *CopilotAdapter
}

func (b copilotCLIBackend) Run(ctx context.Context, req RunRequest) (Outcome, error) {
	return b.adapter.runCLI(ctx, req)
}

// copilotSDKRuntime owns one isolated direct-SDK runtime. Implementations may
// distribute that runtime out of process; callers do not mutate process-global
// environment or derive SDK settings from CLI arguments.
type copilotSDKRuntime interface {
	OpenSession(context.Context, copilotSDKSessionConfig) (copilotSDKSession, error)
}

type copilotSDKSession interface {
	Complete(context.Context, string) (copilotSDKResult, error)
	Close() error
}

type copilotSDKSessionConfig struct {
	Workspace       string
	Model           string
	Options         map[string]string
	ModelCredential string
}

type copilotSDKResult struct {
	Completion             []byte
	Transcript             []byte
	TranscriptTruncated    bool
	TranscriptDroppedBytes int64
}

// copilotSDKBackend is intentionally internal and absent from the registry.
// It establishes the direct session seam without making unfinished SDK
// execution publicly selectable.
type copilotSDKBackend struct {
	runtime copilotSDKRuntime
}

func (b copilotSDKBackend) Run(ctx context.Context, req RunRequest) (out Outcome, runErr error) {
	if b.runtime == nil {
		return Outcome{}, fmt.Errorf("harness: copilot-sdk: no runtime configured")
	}
	if err := validateStandardExecution(req); err != nil {
		return Outcome{}, err
	}
	if req.Workspace == "" {
		return Outcome{}, fmt.Errorf("harness: copilot-sdk: RunRequest.Workspace is empty")
	}
	if len(req.MCPServers) > 0 {
		return Outcome{}, fmt.Errorf("harness: copilot-sdk: external MCP servers are not supported")
	}
	if len(req.Tools) > 0 {
		return Outcome{}, fmt.Errorf("harness: copilot-sdk: tool allowlists are not supported")
	}
	if req.Sandbox != nil {
		return Outcome{}, fmt.Errorf("harness: copilot-sdk: sandbox execution is not supported")
	}
	if req.GoobersIORegistered {
		return Outcome{}, fmt.Errorf("harness: copilot-sdk: goobers-io is not supported")
	}
	if !req.HarnessConfigResolved {
		return Outcome{}, fmt.Errorf("harness: copilot-sdk: configuration must be resolved before execution")
	}

	runCtx, cancel := copilotSessionContext(ctx, req.Timeout)
	defer cancel()

	options, err := normalizeResolvedCopilotConfig(req.Model, req.HarnessOptions)
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: copilot-sdk: invalid resolved configuration: %w", err)
	}
	credential, err := copilotSDKModelCredential(runCtx, req)
	if err != nil {
		return Outcome{}, err
	}
	if req.ValidateCompletion == nil {
		req.ValidateCompletion = func(payload []byte) error {
			return validateCopilotCompletion(req.Mode, payload)
		}
	}
	prompt := renderResponseCompletionPrompt(req)
	session, err := b.runtime.OpenSession(runCtx, copilotSDKSessionConfig{
		Workspace:       req.Workspace,
		Model:           req.Model,
		Options:         options,
		ModelCredential: credential,
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("harness: copilot-sdk: open session: %w", err)
	}
	defer func() {
		runErr = errors.Join(runErr, session.Close())
	}()

	result, err := session.Complete(runCtx, prompt)
	out = Outcome{
		Payload:                result.Completion,
		Transcript:             result.Transcript,
		RenderedPrompt:         []byte(prompt),
		TranscriptTruncated:    result.TranscriptTruncated,
		TranscriptDroppedBytes: result.TranscriptDroppedBytes,
		AgentTelemetryFidelity: "none",
		AgentTelemetryDetail:   "direct Copilot SDK event projection is not implemented",
	}
	if err != nil {
		return out, fmt.Errorf("harness: copilot-sdk: complete session: %w", err)
	}
	if err := validateCompletion(req, result.Completion, nil); err != nil {
		return out, err
	}
	return out, nil
}

func copilotSDKModelCredential(ctx context.Context, req RunRequest) (string, error) {
	modelCapability := string(capability.AgentModel)
	if !slices.Contains(req.Envelope.Capabilities, modelCapability) {
		return "", nil
	}
	if req.Credentials == nil {
		return "", fmt.Errorf("harness: copilot-sdk: %s requires a materialized credential", modelCapability)
	}
	token, err := req.Credentials.Token(ctx, modelCapability)
	if err != nil {
		return "", fmt.Errorf("harness: copilot-sdk: resolve %s credential: %w", modelCapability, err)
	}
	return token, nil
}
