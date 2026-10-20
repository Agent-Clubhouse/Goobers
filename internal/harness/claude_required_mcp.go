package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// claudeRequiredMCPRunner observes the actual Claude session's init event and
// stops the process before its first assistant event when a required server
// has already failed registration or startup. It does not launch a separate
// probe or execute any external MCP tool.
type claudeRequiredMCPRunner struct {
	base    ProcessRunner
	request RunRequest
}

func (r *claudeRequiredMCPRunner) Run(ctx context.Context, req ProcessRequest) (ProcessResult, error) {
	required := requiredDeclaredMCPServerNames(r.request)
	if r.request.GoobersIORegistered {
		required = append(required, goobersIOServerName)
	}
	if len(required) == 0 {
		return r.base.Run(ctx, req)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watcher := &claudeMCPInitWatcher{
		required: required,
		cancel:   cancel,
	}
	if req.StdoutCapture == nil {
		req.StdoutCapture = watcher
	} else {
		req.StdoutCapture = io.MultiWriter(req.StdoutCapture, watcher)
	}
	result, err := r.base.Run(runCtx, req)
	if failure := watcher.failure(); failure != nil {
		return result, errors.Join(failure, err)
	}
	return result, err
}

type claudeMCPInitWatcher struct {
	mu       sync.Mutex
	line     []byte
	required []string
	cancel   context.CancelFunc
	err      error
}

func (w *claudeMCPInitWatcher) Write(p []byte) (int, error) {
	n := len(p)
	w.mu.Lock()
	defer w.mu.Unlock()
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			w.line = append(w.line, p...)
			break
		}
		w.line = append(w.line, p[:end]...)
		w.inspectLine()
		w.line = w.line[:0]
		p = p[end+1:]
	}
	return n, nil
}

func (w *claudeMCPInitWatcher) inspectLine() {
	if w.err != nil {
		return
	}
	var event claudeStreamMessage
	if json.Unmarshal(bytes.TrimSpace(w.line), &event) != nil ||
		event.Type != "system" || event.Subtype != "init" || event.MCPServers == nil {
		return
	}
	statuses := make(map[string]string, len(*event.MCPServers))
	for _, server := range *event.MCPServers {
		statuses[server.Name] = server.Status
	}
	for _, name := range w.required {
		status, found := statuses[name]
		if found && (status == claudeMCPStatusConnected || status == claudeMCPStatusPending) {
			continue
		}
		if !found {
			status = copilotMCPStatusAbsent
		}
		w.err = fmt.Errorf(
			"%w: claude-code refused model dispatch because required MCP server %q failed to start or register (status %q)",
			errRequiredMCPUnavailable, name, status,
		)
		w.cancel()
		return
	}
}

func (w *claudeMCPInitWatcher) failure() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err == nil && len(w.line) > 0 {
		w.inspectLine()
	}
	return w.err
}
