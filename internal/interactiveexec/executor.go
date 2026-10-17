// Package interactiveexec connects experimental sessions to actual local tools
// and the existing Codex harness. Provider access remains mediated by the broker.
package interactiveexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/interactive"
	auth "github.com/goobers/goobers/internal/interactiveauth"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/platform/proc"
)

type Config struct {
	StateDir, SourceDir, Mode string
	Broker                    *auth.Broker
	Principal                 auth.Principal
	TrustedLocalModelLogin    bool
	HeartbeatInterval         time.Duration
	Helper                    func(context.Context, interactive.ExecutionRequest, func(interactive.ExecutionEvent) error) (string, error)
	ReviewChild               func(context.Context, interactive.ExecutionRequest, string, func(string, map[string]any) error) (string, error)
}

type Executor struct {
	config     Config
	mu         sync.Mutex
	runNative  func(context.Context, harness.RunRequest) (harness.Outcome, error)
	runProcess func(context.Context, string, []string, func(harness.TranscriptDelta) error) (harness.ProcessResult, error)
}

// PrepareWorkspace returns a complete private snapshot before Helper config
// pins its path. Initialization must happen inside a leased turn: a config GET
// must never launch untracked workspace writers. No browser path is accepted.
func (e *Executor) PrepareWorkspace(ctx context.Context, owner, sessionID string) (string, error) {
	if owner != e.config.Principal.Identity().Subject {
		return "", fmt.Errorf("workspace owner mismatch")
	}
	path, err := e.workspace(sessionID)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if !e.hasBaseline(path) {
		return "", fmt.Errorf("send a message in this session to initialize its private workspace before using Helper controls")
	}
	return path, nil
}

func (e *Executor) hasBaseline(workspace string) bool {
	base, err := os.ReadFile(e.metadata(workspace, "baseline"))
	return err == nil && regexp.MustCompile(`^[0-9a-f]{40}$`).Match(base)
}

func New(config Config) (*Executor, error) {
	if config.Mode != "local-tools" && config.Mode != "codex-local" {
		return nil, fmt.Errorf("executor must be local-tools or codex-local")
	}
	if config.Mode == "codex-local" && !config.TrustedLocalModelLogin {
		return nil, fmt.Errorf("codex-local requires explicit --trusted-local-model-login; this is a trusted single-user host boundary")
	}
	source, err := filepath.Abs(config.SourceDir)
	if err != nil {
		return nil, err
	}
	config.SourceDir = source
	if info, err := os.Stat(source); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("source directory is unavailable")
	}
	if config.HeartbeatInterval <= 0 {
		config.HeartbeatInterval = 5 * time.Second
	}
	if err := os.MkdirAll(filepath.Join(config.StateDir, "workspaces"), 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(config.StateDir, "metadata"), 0700); err != nil {
		return nil, err
	}
	return &Executor{config: config}, nil
}

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,160}$`)

func (e *Executor) workspace(id string) (string, error) {
	if !safeID.MatchString(id) {
		return "", fmt.Errorf("invalid server session identity")
	}
	return filepath.Join(e.config.StateDir, "workspaces", id), nil
}

// Recover only acknowledges custody after this executor removed its active
// marker on a confirmed process return. A crashed writer is never guessed dead.
func (e *Executor) Recover(_ context.Context, req interactive.ExecutionRequest) error {
	path, err := e.workspace(req.SessionID)
	if err != nil {
		return err
	}
	if req.ChildID != "" {
		if !safeID.MatchString(req.ChildID) {
			return fmt.Errorf("invalid child identity")
		}
		path += "-" + req.ChildID
	}
	if _, err := os.Stat(e.metadata(path, "writer-active")); err == nil {
		return fmt.Errorf("prior writer custody is unconfirmed; isolate the old worker and inspect its workspace before recovery")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (e *Executor) Stop(ctx context.Context, owner, sessionID string) error {
	if owner != e.config.Principal.Identity().Subject {
		return fmt.Errorf("session owner does not match local identity")
	}
	authorities, err := e.config.Broker.ListAuthorities(e.config.Principal)
	if err != nil {
		return err
	}
	for _, a := range authorities {
		if a.Authority.SessionID == sessionID && a.State == "active" {
			if err := e.config.Broker.StopAuthority(ctx, e.config.Principal, a.Authority.Ref); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Executor) Execute(ctx context.Context, req interactive.ExecutionRequest, emit func(interactive.ExecutionEvent) error) (result interactive.ExecutionResult, runErr error) {
	if req.Owner != e.config.Principal.Identity().Subject {
		return result, fmt.Errorf("executor identity mismatch")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	if req.Heartbeat != nil {
		if err := req.Heartbeat(); err != nil {
			return result, err
		}
		go func() {
			ticker := time.NewTicker(e.config.HeartbeatInterval)
			defer ticker.Stop()
			for {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				case <-ticker.C:
					if req.Heartbeat() != nil {
						cancel()
						return
					}
				}
			}
		}()
	}
	workspace, err := e.prepare(ctx, req)
	if err != nil {
		result.CustodyPending = custodyUnconfirmed(err)
		return result, err
	}
	result.WorkspacePath = workspace
	marker := e.metadata(workspace, "writer-active")
	if _, err := os.Stat(marker); err == nil {
		result.CustodyPending = true
		return result, fmt.Errorf("prior writer custody requires reconciliation")
	} else if !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if err = os.WriteFile(marker, []byte(fmt.Sprintf("session=%s turn=%s fence=%d\n", req.SessionID, req.TurnID, req.Fence)), 0600); err != nil {
		return result, err
	}
	// Normal cancellation waits for the process tree through the existing harness
	// runner. A process crash leaves this marker and recovery fails closed.
	defer func() {
		if errors.Is(runErr, proc.ErrQuiescenceUnobservable) || errors.Is(runErr, invoke.ErrWorkspaceNotQuiescent) {
			result.CustodyPending = true
			return
		}
		changes, diff, diffErr := e.changes(context.Background(), workspace)
		runErr = errors.Join(runErr, diffErr)
		if custodyUnconfirmed(diffErr) {
			result.CustodyPending = true
			return
		}
		if err := os.Remove(marker); err != nil {
			result.CustodyPending = true
			runErr = errors.Join(runErr, fmt.Errorf("release writer marker: %w", err))
			return
		}
		result.Changes = changes
		result.Diff = diff
		if req.ReadOnly && len(changes) > 0 && runErr == nil {
			runErr = fmt.Errorf("review child modified its snapshot; result rejected and no parent changes applied")
		}
	}()
	send := func(kind string, data map[string]any) error {
		return emit(interactive.ExecutionEvent{Type: kind, Data: data})
	}
	if err := send("tool.started", map[string]any{"name": "workspace.snapshot", "detail": "Private local copy; no remote publication credentials", "executionMode": e.config.Mode}); err != nil {
		return result, err
	}
	if err := send("tool.completed", map[string]any{"name": "workspace.snapshot", "workspacePath": workspace, "status": "succeeded"}); err != nil {
		return result, err
	}
	providerContext, err := e.providerContext(ctx, req, send)
	if err != nil {
		switch auth.ErrorCode(err) {
		case "interaction_required", "consent_required", "connection_revoked", "authority_expired":
			result.WaitingAuth = true
			result.AuthReason = err.Error()
			return result, nil
		default:
			return result, err
		}
	}
	if req.Mode == "helper" && req.ChildID == "" && !req.ReadOnly && e.config.Helper != nil {
		result.Message, runErr = e.config.Helper(ctx, req, emit)
		if result.Message != "" || runErr != nil {
			return result, runErr
		}
	}
	if e.config.Mode == "codex-local" {
		result.Message, runErr = e.native(ctx, req, workspace, providerContext, send)
	} else {
		result.Message, runErr = e.local(ctx, req, workspace, providerContext, send)
	}
	return result, runErr
}

func (e *Executor) prepare(ctx context.Context, req interactive.ExecutionRequest) (prepared string, prepareErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	workspace, err := e.workspace(req.SessionID)
	if err != nil {
		return "", err
	}
	source := e.config.SourceDir
	if req.ChildID != "" {
		if !safeID.MatchString(req.ChildID) {
			return "", fmt.Errorf("invalid child identity")
		}
		source = workspace
		workspace = filepath.Join(e.config.StateDir, "workspaces", req.SessionID+"-"+req.ChildID)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".git")); err == nil {
		if !e.hasBaseline(workspace) {
			if _, markerErr := os.Stat(e.metadata(workspace, "writer-active")); markerErr == nil {
				return "", fmt.Errorf("snapshot initialization has unknown writer custody: %w", invoke.ErrWorkspaceNotQuiescent)
			}
			return "", fmt.Errorf("snapshot baseline is incomplete; use a new session or reconcile initialization")
		}
		return workspace, nil
	}
	marker := e.metadata(workspace, "writer-active")
	file, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("prior preparation writer custody is unconfirmed: %w", invoke.ErrWorkspaceNotQuiescent)
	}
	if err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	defer func() {
		if custodyUnconfirmed(prepareErr) {
			return
		}
		if err := os.Remove(marker); err != nil {
			prepareErr = errors.Join(prepareErr, invoke.ErrWorkspaceNotQuiescent, fmt.Errorf("release preparation marker: %w", err))
		}
	}()
	if err := os.MkdirAll(workspace, 0700); err != nil {
		return "", err
	}
	if err := copySource(source, workspace); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(workspace, ".goobers"), 0700); err != nil {
		return "", err
	}
	ignore, _ := os.ReadFile(filepath.Join(workspace, ".gitignore"))
	ignore = append(ignore, []byte("\n.goobers/\n.home/\n.tmp/\n")...)
	if err := os.WriteFile(filepath.Join(workspace, ".gitignore"), ignore, 0600); err != nil {
		return "", err
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "--all"}, {"-c", "user.name=Goobers Prototype", "-c", "user.email=prototype@localhost", "-c", "commit.gpgsign=false", "commit", "-qm", "Session baseline"}} {
		if _, err := e.command(ctx, workspace, append([]string{"git"}, args...), nil); err != nil {
			return "", err
		}
	}
	base, err := e.command(ctx, workspace, []string{"git", "rev-parse", "HEAD"}, nil)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(e.metadata(workspace, "baseline"), []byte(strings.TrimSpace(string(base.Transcript))), 0600); err != nil {
		return "", err
	}
	return workspace, nil
}

func (e *Executor) metadata(workspace, suffix string) string {
	return filepath.Join(e.config.StateDir, "metadata", filepath.Base(workspace)+"."+suffix)
}

func copySource(source, dest string) error {
	count := 0
	var total int64
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor") {
			return filepath.SkipDir
		}
		if !entry.IsDir() && ((strings.HasPrefix(name, ".") && name != ".gitignore" && name != ".gitattributes" && name != ".editorconfig") || sensitiveFilename(name)) {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("workspace snapshot refuses symlink %s", rel)
		}
		target := filepath.Join(dest, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("workspace snapshot refuses special file %s", rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		count++
		total += info.Size()
		if count > 10000 || total > 64<<20 || info.Size() > 4<<20 {
			return fmt.Errorf("workspace snapshot exceeds prototype bound")
		}
		// Copy files, never hardlink the source or transport its Git credentials/hooks.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm()&0700|0600)
	})
}

func sensitiveFilename(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".pem") || strings.HasSuffix(lower, ".key") || strings.HasSuffix(lower, ".pfx") || strings.HasSuffix(lower, ".p12") || lower == "credentials" || lower == "auth.json" || strings.HasPrefix(lower, "id_rsa") || strings.HasPrefix(lower, "id_ed25519") || strings.HasPrefix(lower, "secrets.")
}

func (e *Executor) authority(ctx context.Context, req interactive.ExecutionRequest) (auth.Authority, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	authorities, err := e.config.Broker.ListAuthorities(e.config.Principal)
	if err != nil {
		return auth.Authority{}, err
	}
	var parent *auth.Authority
	for _, a := range authorities {
		if a.Authority.SessionID == req.SessionID {
			if a.Authority.ExecutionID == req.SessionID && (parent == nil || a.Authority.Ref.Version > parent.Ref.Version) {
				v := a.Authority
				parent = &v
			}
			if req.ChildID != "" && a.Authority.ExecutionID == req.ChildID {
				return a.Authority, nil
			}
		}
	}
	if parent == nil {
		connections, err := e.config.Broker.ListConnections(e.config.Principal)
		if err != nil {
			return auth.Authority{}, err
		}
		var bindings []auth.Binding
		for _, c := range connections {
			bindings = append(bindings, auth.Binding{Connection: c.Ref, Resources: c.Resources})
		}
		a, err := e.config.Broker.IssueAuthority(ctx, e.config.Principal, auth.AuthorityRequest{SessionID: req.SessionID, ExecutionID: req.SessionID, ExecutionFence: req.Fence, Bindings: bindings, AllowedActions: []auth.Action{auth.ReadWorkItem, auth.QueryTelemetry}, NotAfter: time.Now().Add(2 * time.Hour), Budget: 80, MaxChildren: 3, MaxDepth: 1})
		if err != nil {
			return auth.Authority{}, err
		}
		parent = &a
	}
	if req.ChildID == "" {
		if req.Fence > parent.ExecutionFence {
			return e.config.Broker.RebindExecution(ctx, e.config.Principal, parent.Ref, req.Fence)
		}
		return *parent, nil
	}
	return e.config.Broker.DeriveChild(ctx, e.config.Principal, parent.Ref, auth.AuthorityRequest{SessionID: req.SessionID, ExecutionID: req.ChildID, ExecutionFence: req.Fence, Bindings: parent.Bindings, AllowedActions: parent.AllowedActions, NotAfter: parent.NotAfter, Budget: 10, MaxChildren: 0, MaxDepth: 1})
}

func (e *Executor) providerContext(ctx context.Context, req interactive.ExecutionRequest, emit func(string, map[string]any) error) (string, error) {
	a, err := e.authority(ctx, req)
	if err != nil {
		return "", err
	}
	queries := []struct {
		action   auth.Action
		resource auth.Resource
		payload  auth.Payload
	}{{auth.ReadWorkItem, auth.DemoADOResource(), auth.Payload{WorkItemID: 427}}, {auth.QueryTelemetry, auth.DemoInsightsResource(), auth.Payload{Query: "requests | summarize count() by resultCode"}}}
	var parts []string
	previous, err := e.config.Broker.ListOperations(e.config.Principal, req.SessionID)
	if err != nil {
		return "", err
	}
	for i, q := range queries {
		if err := emit("tool.started", map[string]any{"name": string(q.action), "providerMode": auth.ControlledLocal}); err != nil {
			return "", err
		}
		id := fmt.Sprintf("%s-%s-provider-%d", req.TurnID, req.ChildID, i)
		// Recovery does not redispatch a previously confirmed operation under a
		// fresh authority revision. Its receipt is historical evidence, not a new
		// provider read. Unknown outcomes remain blocked for reconciliation.
		var prior *auth.Operation
		for j := range previous {
			if previous[j].ID == id {
				prior = &previous[j]
				break
			}
		}
		if prior != nil {
			if prior.Authority.ID != a.Ref.ID || prior.Action != q.action || prior.Resource != q.resource || prior.ExecutionID != a.ExecutionID {
				return "", fmt.Errorf("provider receipt does not match recovered execution")
			}
			if prior.State != "confirmed" {
				return "", fmt.Errorf("prior provider operation %s requires reconciliation; no automatic replay", prior.State)
			}
			data, _ := json.Marshal(prior.Result)
			parts = append(parts, string(data))
			if err := emit("tool.completed", map[string]any{"name": string(q.action), "status": prior.State, "providerMode": prior.Mode, "operationId": prior.ID, "result": prior.Result, "replayedReceipt": true}); err != nil {
				return "", err
			}
			continue
		}
		op, err := e.config.Broker.Execute(ctx, e.config.Principal, auth.OperationRequest{Authority: a.Ref, SessionID: req.SessionID, ExecutionID: a.ExecutionID, ExecutionFence: req.Fence, TurnID: req.TurnID, LeaseID: req.LeaseID, OperationID: id, ActionID: id, IdempotencyKey: id, Action: q.action, Resource: q.resource, Payload: q.payload})
		if err != nil {
			return "", err
		}
		if op.State != "confirmed" {
			return "", fmt.Errorf("provider operation %s: %s", op.State, op.Message)
		}
		data, _ := json.Marshal(op.Result)
		parts = append(parts, string(data))
		if err := emit("tool.completed", map[string]any{"name": string(q.action), "status": op.State, "providerMode": op.Mode, "operationId": op.ID, "result": op.Result}); err != nil {
			return "", err
		}
	}
	return strings.Join(parts, "\n"), nil
}

func run(ctx context.Context, workspace string, command []string, checkpoint func(harness.TranscriptDelta) error) (harness.ProcessResult, error) {
	for _, dir := range []string{".home", ".tmp"} {
		if err := os.MkdirAll(filepath.Join(workspace, dir), 0700); err != nil {
			return harness.ProcessResult{}, err
		}
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(workspace, ".home"), "TMPDIR=" + filepath.Join(workspace, ".tmp"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "NODE_OPTIONS="}
	ctx, proof := invoke.WithWorkspaceQuiescence(ctx)
	result, err := (harness.ExecProcessRunner{}).Run(ctx, harness.ProcessRequest{Command: command, Dir: workspace, Env: env, Timeout: 2 * time.Minute, MaxTranscriptBytes: 64 << 10, TranscriptCheckpoint: checkpoint, TranscriptCheckpointInterval: 100 * time.Millisecond})
	return result, verifyWriters(proof, err)
}

func verifyWriters(proof *invoke.WorkspaceQuiescence, err error) error {
	if err != nil && !proof.HasRegisteredWriters() {
		return err
	}
	return errors.Join(err, proof.Verify())
}

func (e *Executor) command(ctx context.Context, workspace string, command []string, checkpoint func(harness.TranscriptDelta) error) (harness.ProcessResult, error) {
	if e.runProcess != nil {
		return e.runProcess(ctx, workspace, command, checkpoint)
	}
	return run(ctx, workspace, command, checkpoint)
}

func (e *Executor) changes(ctx context.Context, workspace string) ([]interactive.Change, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	base, err := os.ReadFile(e.metadata(workspace, "baseline"))
	if err != nil {
		return nil, "", fmt.Errorf("baseline custody unavailable")
	}
	result, err := e.command(ctx, workspace, []string{"git", "--no-pager", "diff", "--no-ext-diff", "--no-textconv", string(base), "--", ".", ":!.goobers"}, nil)
	if err != nil {
		return nil, "", err
	}
	status, err := e.command(ctx, workspace, []string{"git", "diff", "--name-status", string(base)}, nil)
	if err != nil {
		return nil, "", err
	}
	changes := []interactive.Change{}
	for _, line := range strings.Split(string(status.Transcript), "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 {
			changes = append(changes, interactive.Change{Path: parts[1], Status: parts[0]})
		}
	}
	untracked, err := e.command(ctx, workspace, []string{"git", "ls-files", "--others", "--exclude-standard"}, nil)
	if err != nil {
		return nil, "", err
	}
	diff := string(result.Transcript)
	for _, name := range strings.Split(strings.TrimSpace(string(untracked.Transcript)), "\n") {
		if name == "" {
			continue
		}
		changes = append(changes, interactive.Change{Path: name, Status: "added"})
		content, err := readWorkspace(workspace, name)
		if err != nil {
			diff += "\nUntracked file " + name + ": content unavailable or outside safe display limits\n"
			continue
		}
		diff += "\ndiff --git a/" + name + " b/" + name + "\nnew file\n"
		for _, line := range strings.Split(content, "\n") {
			diff += "+" + line + "\n"
		}
		if len(diff) > 128<<10 {
			diff = bounded(diff, 128<<10)
			break
		}
	}
	return changes, diff, nil
}
