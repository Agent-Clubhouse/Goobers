package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const launcherContractFlag = "--goobers-launcher-contract"

var verifiedAdapterManagedLaunchers sync.Map

// launcherContract is the versioned, credential-free wrapper handshake. It is
// deliberately not a new harness: prompts, results, tools and native transcript
// events still obey the existing Copilot adapter contract.
type launcherContract struct {
	Version     int      `json:"version"`
	SessionMode string   `json:"sessionMode"`
	SessionArgs []string `json:"sessionArgs,omitempty"`
}

func (c *CopilotAdapter) prepareLauncherSession(ctx context.Context, workspace string, argv, env []string) ([]string, []string, string, func(), error) {
	cleanup := func() {}
	contract, err := c.launcherSessionContract(ctx)
	if err != nil {
		return nil, nil, "", cleanup, err
	}
	if c.RequireLauncherContract && !c.isLauncherContractVerified() {
		return nil, nil, "", cleanup, fmt.Errorf("harness: copilot launcher adapter-managed fallback was not verified by preflight")
	}
	if copilotCommandSelectsSession(argv) {
		if contract.SessionMode != "adapter-managed" {
			return nil, nil, "", cleanup, fmt.Errorf("launcher session arguments conflict with %s session ownership", contract.SessionMode)
		}
		return argv, env, "", cleanup, nil
	}
	if contract.SessionMode == "wrapper-managed" {
		// The wrapper owns its internal ID, but must export native session events
		// to this private per-invocation path. Never guess its newest session.
		dir, err := os.MkdirTemp(workspace, ".goobers-launcher-session-")
		if err != nil {
			return nil, nil, "", cleanup, err
		}
		transcript := filepath.Join(dir, "native.jsonl")
		return argv, overrideEnv(env, "GOOBERS_SESSION_TRANSCRIPT", transcript), transcript, func() { _ = os.RemoveAll(dir) }, nil
	}
	id, err := newHarnessSessionID()
	if err != nil {
		return nil, nil, "", cleanup, fmt.Errorf("create transcript capture id: %w", err)
	}
	if contract.SessionMode == "templated" {
		for _, arg := range contract.SessionArgs {
			argv = append(argv, strings.ReplaceAll(arg, "{sessionId}", id))
		}
	} else {
		argv = append(argv, "--session-id", id)
	}
	transcript := ""
	if home, ok := copilotConfigHome(env); ok {
		transcript = copilotSessionLogPath(home, id)
	}
	return argv, env, transcript, cleanup, nil
}

func parseLauncherContract(data []byte) (launcherContract, error) {
	var contract launcherContract
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&contract); err != nil {
		return contract, fmt.Errorf("invalid launcher contract: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return contract, fmt.Errorf("launcher contract must be exactly one JSON object")
	}
	if contract.Version != 1 {
		return contract, fmt.Errorf("unsupported launcher contract version %d", contract.Version)
	}
	switch contract.SessionMode {
	case "adapter-managed", "wrapper-managed":
		if len(contract.SessionArgs) != 0 {
			return contract, fmt.Errorf("sessionArgs is only valid for templated sessions")
		}
	case "templated":
		if len(contract.SessionArgs) == 0 || len(contract.SessionArgs) > 16 {
			return contract, fmt.Errorf("templated sessionArgs must contain 1 to 16 arguments")
		}
		found := false
		for _, arg := range contract.SessionArgs {
			if arg == "" || len(arg) > 1024 || strings.ContainsRune(arg, 0) {
				return contract, fmt.Errorf("invalid session argument")
			}
			found = found || strings.Contains(arg, "{sessionId}")
			remaining := strings.ReplaceAll(arg, "{sessionId}", "")
			if strings.ContainsAny(remaining, "{}") {
				return contract, fmt.Errorf("session arguments support only {sessionId}")
			}
		}
		if !found {
			return contract, fmt.Errorf("templated sessionArgs must use {sessionId}")
		}
	default:
		return contract, fmt.Errorf("unsupported launcher sessionMode %q", contract.SessionMode)
	}
	return contract, nil
}

func (c *CopilotAdapter) launcherSessionContract(ctx context.Context) (launcherContract, error) {
	if !c.RequireLauncherContract {
		return launcherContract{Version: 1, SessionMode: "adapter-managed"}, nil
	}
	c.launcherMu.Lock()
	defer c.launcherMu.Unlock()
	if c.launcherContract != nil {
		return *c.launcherContract, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := append(append([]string(nil), resolveHarnessCommand(c.Command)...), launcherContractFlag)
	stdout := newTranscriptBuffer(16 * 1024)
	result, err := c.runner().Run(probeCtx, ProcessRequest{
		Command: command, Env: baseEnv(nil), Timeout: 10 * time.Second,
		MaxTranscriptBytes: 16 * 1024,
		StdoutCapture:      stdout,
	})
	contractOutput := bytes.TrimSpace(stdout.Bytes())
	nonJSONOutput := len(contractOutput) > 0 && contractOutput[0] != '{'
	if err != nil || result.ExitCode != 0 || stdout.Truncated() || len(contractOutput) == 0 || nonJSONOutput {
		handshakeAbsent := result.ExitCode > 0 || len(contractOutput) == 0 || nonJSONOutput
		if handshakeAbsent && !stdout.Truncated() &&
			!errors.Is(err, ErrTimeout) && !errors.Is(err, ErrCanceled) &&
			c.AllowAdapterManagedFallback {
			contract := launcherContract{Version: 1, SessionMode: "adapter-managed"}
			c.launcherContract = &contract
			c.launcherContractVerified = adapterManagedLauncherVerified(c.Command)
			return contract, nil
		}
		return launcherContract{}, fmt.Errorf("harness: copilot launcher is incompatible: %s must return a bounded version-1 session contract without starting an agent; use direct copilot or a contract-aware wrapper", launcherContractFlag)
	}
	contract, err := parseLauncherContract(contractOutput)
	if err != nil {
		return launcherContract{}, fmt.Errorf("harness: copilot launcher is incompatible: %w; use direct copilot or a contract-aware wrapper", err)
	}
	if contract.SessionMode != "adapter-managed" && (copilotCommandSelectsSession(c.Command) || copilotCommandSelectsSession(c.ExtraArgs)) {
		return launcherContract{}, fmt.Errorf("harness: copilot launcher is incompatible: configured session selectors conflict with %s ownership", contract.SessionMode)
	}
	c.launcherContract = &contract
	c.launcherContractVerified = true
	return contract, nil
}

func (c *CopilotAdapter) cacheLauncherContract(contract launcherContract) {
	c.launcherMu.Lock()
	defer c.launcherMu.Unlock()
	c.launcherContract = &contract
	c.launcherContractVerified = true
	if contract.SessionMode == "adapter-managed" {
		markAdapterManagedLauncherVerified(c.Command)
	}
}

func (c *CopilotAdapter) isLauncherContractVerified() bool {
	c.launcherMu.Lock()
	defer c.launcherMu.Unlock()
	return c.launcherContractVerified
}

func markAdapterManagedLauncherVerified(command []string) {
	verifiedAdapterManagedLaunchers.Store(strings.Join(command, "\x00"), struct{}{})
}

func adapterManagedLauncherVerified(command []string) bool {
	_, ok := verifiedAdapterManagedLaunchers.Load(strings.Join(command, "\x00"))
	return ok
}
