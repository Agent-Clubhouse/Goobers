package interactiveexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/interactive"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/platform/proc"
)

// local is a transparent deterministic tool driver. It never poses as a model.
// Its small command vocabulary makes the entire execution journey runnable
// without model credentials, while every file read, edit, and test is real.
func (e *Executor) local(ctx context.Context, req interactive.ExecutionRequest, workspace, providerContext string, emit func(string, map[string]any) error) (string, error) {
	text := strings.ToLower(strings.TrimSpace(req.Text))
	r := &localRun{executor: e, ctx: ctx, request: req, workspace: workspace, emit: emit, steers: []string{}}
	if err := r.emit("assistant.message", map[string]any{"text": "Local tools executor: actual filesystem and test commands; no language model. ADO and telemetry context are controlled local fixtures."}); err != nil {
		return "", err
	}
	if r.request.ChildID != "" || r.request.ReadOnly {
		return r.reviewChild()
	}
	if strings.HasPrefix(text, "wait") {
		return r.wait()
	}
	if strings.Contains(text, "investigat") || strings.Contains(text, "inspect") || strings.Contains(text, "context") {
		return r.investigate(providerContext)
	}
	if strings.Contains(text, "fix") || strings.Contains(text, "edit") {
		return r.edit()
	}
	if strings.Contains(text, "test") {
		return r.test()
	}
	if strings.Contains(text, "review") || strings.Contains(text, "diff") {
		return r.diff()
	}
	if strings.HasPrefix(text, "read ") {
		name := strings.TrimSpace(r.request.Text[5:])
		content, err := readWorkspace(r.workspace, name)
		if err != nil {
			return "", err
		}
		return content, nil
	}
	return "Available actual local commands: investigate, fix (the sample invoice bug), test, review, read <relative path>, and wait. Helper also supports proposing validated workflows and runs. Select codex-local at server startup for a natural-language coding agent.", nil
}

type localRun struct {
	executor  *Executor
	ctx       context.Context
	request   interactive.ExecutionRequest
	workspace string
	emit      func(string, map[string]any) error
	steers    []string
}

func (r *localRun) boundary(duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-timer.C:
			return nil
		case steer, ok := <-r.request.Steering:
			if !ok {
				return nil
			}
			r.steers = append(r.steers, steer.Text)
			acknowledge(steer, "steer", nil)
			if err := r.emit("assistant.message", map[string]any{"text": "Steering applied at the next tool boundary: " + steer.Text}); err != nil {
				return err
			}
		}
	}
}

func (r *localRun) tool(name string, command []string) (string, error) {
	if err := r.boundary(250 * time.Millisecond); err != nil {
		return "", err
	}
	if err := r.emit("tool.started", map[string]any{"name": name, "command": command, "executionMode": "local-tools"}); err != nil {
		return "", err
	}
	result, err := r.executor.command(r.ctx, r.workspace, command, func(delta harness.TranscriptDelta) error {
		if len(delta.Data) == 0 {
			return nil
		}
		return r.emit("tool.output", map[string]any{"name": name, "text": string(delta.Data)})
	})
	status := "succeeded"
	if err != nil {
		status = "failed"
	}
	if emitErr := r.emit("tool.completed", map[string]any{"name": name, "status": status, "exitCode": result.ExitCode, "output": string(result.Transcript)}); emitErr != nil {
		return "", errors.Join(err, emitErr)
	}
	return string(result.Transcript), err
}

func (r *localRun) reviewChild() (string, error) {
	if r.executor.config.ReviewChild != nil {
		return r.executor.config.ReviewChild(r.ctx, r.request, r.workspace, r.emit)
	}
	output, err := r.tool("child.review.tests", []string{"node", "--test"})
	if r.ctx.Err() != nil || custodyUnconfirmed(err) {
		return "", errors.Join(r.ctx.Err(), err)
	}
	if err != nil {
		return "Read-only child review found failing tests in its independent snapshot. Parent files were not changed.\n" + output, nil
	}
	return "Read-only child review: all tests pass in an independent snapshot. No changes were applied to the parent.\n" + output, nil
}

func (r *localRun) wait() (string, error) {
	if err := r.emit("tool.started", map[string]any{"name": "bounded.wait", "detail": "Waiting up to 45 seconds; steering and stop remain active"}); err != nil {
		return "", err
	}
	if err := r.boundary(45 * time.Second); err != nil {
		return "", err
	}
	return "Bounded wait completed.", nil
}

func (r *localRun) investigate(providerContext string) (string, error) {
	if err := r.boundary(1500 * time.Millisecond); err != nil {
		return "", err
	}
	info, err := readWorkspace(r.workspace, "invoice.mjs")
	if err != nil {
		return "", err
	}
	if err := r.emit("tool.completed", map[string]any{"name": "repository.read", "path": "invoice.mjs", "output": info}); err != nil {
		return "", err
	}
	output, testErr := r.tool("repository.test", []string{"node", "--test"})
	if r.ctx.Err() != nil || custodyUnconfirmed(testErr) {
		return "", errors.Join(r.ctx.Err(), testErr)
	}
	result := "The repository test was executed successfully."
	if testErr != nil {
		result = "The repository test reproduced the failure. Inspect the tool output before choosing a fix."
	}
	return result + "\n\nSource evidence (invoice.mjs):\n" + info + "\nControlled provider observations:\n" + providerContext + "\nTest output:\n" + output, nil
}

func (r *localRun) edit() (string, error) {
	if err := r.boundary(1500 * time.Millisecond); err != nil {
		return "", err
	}
	for _, s := range r.steers {
		lower := strings.ToLower(s)
		if strings.Contains(lower, "do not edit") || strings.Contains(lower, "don't edit") || strings.Contains(lower, "read only") {
			return "Steering requested read-only work. No edit was applied.", nil
		}
	}
	content, err := readWorkspace(r.workspace, "invoice.mjs")
	if err != nil {
		return "", err
	}
	const before = "return subtotal - discount;"
	const after = "return Math.max(0, subtotal - discount);"
	if !strings.Contains(content, before) && !strings.Contains(content, after) {
		return "The known sample edit does not match this repository. Use codex-local for arbitrary code changes; no file was changed.", nil
	}
	if strings.Contains(content, before) {
		if err := r.emit("tool.started", map[string]any{"name": "repository.edit", "path": "invoice.mjs", "before": before, "after": after}); err != nil {
			return "", err
		}
		root, err := os.OpenRoot(r.workspace)
		if err != nil {
			return "", err
		}
		// Best-effort cleanup; the operation result is reported separately.
		defer func() { _ = root.Close() }()
		file, err := root.OpenFile("invoice.mjs", os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return "", err
		}
		_, writeErr := file.WriteString(strings.Replace(content, before, after, 1))
		closeErr := file.Close()
		if writeErr != nil {
			return "", writeErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		if err := r.emit("tool.completed", map[string]any{"name": "repository.edit", "status": "succeeded", "path": "invoice.mjs"}); err != nil {
			return "", err
		}
	}
	output, err := r.tool("repository.test", []string{"node", "--test"})
	if err != nil {
		return "The edit is preserved, but tests failed.\n" + output, err
	}
	return "Updated invoice.mjs to clamp negative totals to zero, preserving the exported API. The actual Node test suite passed. Review the cumulative diff before publication.\n" + output, nil
}

func (r *localRun) test() (string, error) {
	output, err := r.tool("repository.test", []string{"node", "--test"})
	if err != nil {
		return "Tests failed; the actual test output is preserved.\n" + output, err
	}
	return "All actual repository tests passed.\n" + output, nil
}

func (r *localRun) diff() (string, error) {
	if err := r.boundary(250 * time.Millisecond); err != nil {
		return "", err
	}
	_, output, err := r.executor.changes(r.ctx, r.workspace)
	if err != nil {
		return "", err
	}
	if err := r.emit("tool.completed", map[string]any{"name": "repository.diff", "status": "succeeded", "output": output}); err != nil {
		return "", err
	}
	if output == "" {
		return "The session has no tracked changes from its baseline.", nil
	}
	return "Cumulative changes from the session baseline:\n" + output, nil
}

func custodyUnconfirmed(err error) bool {
	return errors.Is(err, invoke.ErrWorkspaceNotQuiescent) || errors.Is(err, proc.ErrQuiescenceUnobservable)
}

func readWorkspace(workspace, name string) (string, error) {
	if filepath.IsAbs(name) || strings.HasPrefix(name, ".") || strings.Contains(name, "/.") {
		return "", fmt.Errorf("hidden and absolute paths are outside local read tools")
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return "", err
	}
	// Best-effort cleanup; the operation result is reported separately.
	defer func() { _ = root.Close() }()
	info, err := root.Stat(name)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > 128<<10 {
		return "", fmt.Errorf("file is outside read limits")
	}
	data, err := root.ReadFile(name)
	return string(data), err
}
