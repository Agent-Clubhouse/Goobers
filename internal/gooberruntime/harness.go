package gooberruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// HarnessRequest is the payload supplied to the agent harness.
type HarnessRequest struct {
	Context     GooberContext        `json:"context"`
	Environment ExecutionEnvironment `json:"environment"`
}

// Harness invokes a goober task or reviewer through an agent harness.
type Harness interface {
	Invoke(context.Context, HarnessRequest) (apiv1.ResultEnvelope, error)
	Review(context.Context, HarnessRequest) (apiv1.Verdict, error)
}

// ErrHarnessUnavailable is returned when no Copilot harness command is
// configured for the runtime.
var ErrHarnessUnavailable = errors.New("copilot harness command is not configured")

// ProcessRunner runs the concrete harness process.
type ProcessRunner interface {
	Run(context.Context, ProcessRequest) ([]byte, error)
}

// ProcessRequest describes a harness process execution.
type ProcessRequest struct {
	Command []string
	Dir     string
	Env     map[string]string
	Stdin   []byte
}

// CopilotHarness is the v1 GitHub Copilot harness adapter. The deployment
// supplies the concrete command; the adapter sends JSON on stdin and expects a
// result/verdict JSON envelope on stdout.
type CopilotHarness struct {
	Command []string
	Runner  ProcessRunner
}

// NewCopilotHarness constructs a Copilot harness adapter.
func NewCopilotHarness(command []string) *CopilotHarness {
	return &CopilotHarness{Command: append([]string(nil), command...), Runner: ExecProcessRunner{}}
}

// Invoke runs an agentic task through the Copilot harness command.
func (h *CopilotHarness) Invoke(ctx context.Context, req HarnessRequest) (apiv1.ResultEnvelope, error) {
	var out apiv1.ResultEnvelope
	if err := h.run(ctx, "invoke", req, &out); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return out, nil
}

// Review runs an agentic reviewer gate through the Copilot harness command.
func (h *CopilotHarness) Review(ctx context.Context, req HarnessRequest) (apiv1.Verdict, error) {
	var out apiv1.Verdict
	if err := h.run(ctx, "review", req, &out); err != nil {
		return apiv1.Verdict{}, err
	}
	return out, nil
}

func (h *CopilotHarness) run(ctx context.Context, mode string, req HarnessRequest, out interface{}) error {
	if len(h.Command) == 0 {
		return ErrHarnessUnavailable
	}
	runner := h.Runner
	if runner == nil {
		runner = ExecProcessRunner{}
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal harness request: %w", err)
	}
	command := append(append([]string(nil), h.Command...), mode)
	stdout, err := runner.Run(ctx, ProcessRequest{
		Command: command,
		Dir:     req.Environment.RepoDir,
		Env:     req.Environment.Env,
		Stdin:   payload,
	})
	if err != nil {
		return fmt.Errorf("run copilot harness: %w", err)
	}
	if err := json.Unmarshal(stdout, out); err != nil {
		return fmt.Errorf("decode copilot harness response: %w", err)
	}
	return nil
}

// ExecProcessRunner runs a harness command with os/exec.
type ExecProcessRunner struct{}

// Run executes the requested process and returns stdout.
func (ExecProcessRunner) Run(ctx context.Context, req ProcessRequest) ([]byte, error) {
	if len(req.Command) == 0 {
		return nil, ErrHarnessUnavailable
	}
	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
	cmd.Dir = req.Dir
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stdin = bytes.NewReader(req.Stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if stderr.Len() > 0 {
			return nil, fmt.Errorf("%w: %s", err, stderr.String())
		}
		return nil, err
	}
	return out, nil
}

// Evaluator runs a gate review over prepared context.
type Evaluator interface {
	Evaluate(context.Context, HarnessRequest) (apiv1.Verdict, error)
}

// HarnessEvaluator evaluates agentic gates by asking the harness for a verdict.
type HarnessEvaluator struct {
	Harness Harness
}

// Evaluate returns a reviewer verdict from the configured harness.
func (e HarnessEvaluator) Evaluate(ctx context.Context, req HarnessRequest) (apiv1.Verdict, error) {
	if e.Harness == nil {
		return apiv1.Verdict{}, ErrHarnessUnavailable
	}
	return e.Harness.Review(ctx, req)
}
