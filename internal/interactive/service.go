package interactive

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/secretpattern"
)

type activeRun struct {
	cancel   context.CancelFunc
	request  ExecutionRequest
	steering chan Steering
	controls chan DriverControl
}

// Service coordinates durable sessions, execution leases, and command admission.
type Service struct {
	mu         sync.Mutex
	reloadMu   sync.Mutex
	reloading  bool
	opts       Options
	state      diskState
	active     map[string]map[string]*activeRun
	changed    chan struct{}
	held       *lock.Handle
	scrubber   *secretpattern.Scrubber
	wg         sync.WaitGroup
	closed     bool
	stopSweep  chan struct{}
	policyWake chan struct{}
	fatal      error
}

// Open loads session state and acquires exclusive coordinator ownership.
func Open(opts Options) (*Service, error) {
	if opts.StateDir == "" {
		return nil, errors.New("interactive StateDir is required")
	}
	if opts.Principal == nil {
		return nil, errors.New("interactive Principal resolver is required")
	}
	if opts.AvailableExecutionModes == nil {
		opts.AvailableExecutionModes = []string{ExecutionModeLocalTools}
	}
	opts.AvailableExecutionModes = append([]string{}, opts.AvailableExecutionModes...)
	if opts.DefaultExecutionMode == "" {
		opts.DefaultExecutionMode = ExecutionModeLocalTools
	}
	modes := map[string]bool{}
	for _, mode := range opts.AvailableExecutionModes {
		if (mode != ExecutionModeLocalTools && mode != ExecutionModeCodexLocal) || modes[mode] {
			return nil, errors.New("invalid or duplicate interactive execution mode")
		}
		modes[mode] = true
	}
	if opts.DefaultExecutionMode != ExecutionModeLocalTools && opts.DefaultExecutionMode != ExecutionModeCodexLocal {
		return nil, errors.New("invalid interactive default execution mode")
	}
	if opts.MaxTurnDuration <= 0 {
		opts.MaxTurnDuration = 10 * time.Minute
	}
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = 30 * time.Second
	}
	if opts.WorkerID == "" {
		opts.WorkerID = id("worker_")
	}
	if opts.MaxActive <= 0 {
		opts.MaxActive = 4
	}
	if opts.MaxActivePerOwner <= 0 {
		opts.MaxActivePerOwner = 2
	}
	if opts.Now == nil {
		opts.Now = func() time.Time { return time.Now().UTC() }
	}
	if opts.MaxSessionOutputBytes <= 0 {
		opts.MaxSessionOutputBytes = 8 << 20
	}
	var err error
	opts.Surfaces, err = prepareSurfaces(opts.Surfaces)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.StateDir, 0700); err != nil {
		return nil, err
	}
	held, err := lock.TryAcquire(filepath.Join(opts.StateDir, "sessions.lock"))
	if err != nil {
		return nil, fmt.Errorf("interactive coordinator already owned or inaccessible: %w", err)
	}
	s := &Service{opts: opts, state: emptyState(), active: map[string]map[string]*activeRun{}, changed: make(chan struct{}), held: held, scrubber: secretpattern.NewScrubber(), stopSweep: make(chan struct{}), policyWake: make(chan struct{}, 1)}
	b, err := os.ReadFile(filepath.Join(opts.StateDir, "sessions.json"))
	if err == nil {
		err = json.Unmarshal(b, &s.state)
	} else if os.IsNotExist(err) {
		err = nil
	}
	if err != nil || s.state.Version != 1 || s.state.Sessions == nil || s.state.Creates == nil {
		return nil, errors.Join(fmt.Errorf("invalid interactive state: %w", err), held.Release())
	}
	// Recovery never silently relaunches an execution or an external operation.
	for _, rec := range s.state.Sessions {
		if rec.Session.Status == "running" || rec.Session.Status == "stopping" {
			rec.Session.Fence++
			rec.Session.Status = "recovery_required"
			rec.Session.RecoveryReason = "Coordinator restarted; prior writer custody and external effects must be reconciled before resuming."
			for i := range rec.Session.Turns {
				if rec.Session.Turns[i].Status == "running" {
					rec.Session.Turns[i].Status = "interrupted"
				}
			}
			appendEvent(rec, "recovery.required", rec.Session.ActiveTurnID, "", map[string]any{"reason": rec.Session.RecoveryReason})
		}
		for i := range rec.Session.Children {
			if rec.Session.Children[i].Status == "running" {
				rec.Session.Children[i].Status = "uncertain"
				rec.Session.Status = "recovery_required"
				rec.Session.Fence++
				rec.Session.RecoveryReason = "Interrupted child requires custody and effect reconciliation; it will not be replayed."
				appendEvent(rec, "recovery.required", "", "", map[string]any{"childId": rec.Session.Children[i].ID, "reason": rec.Session.RecoveryReason})
			}
		}
		for key, cmd := range rec.Commands {
			if cmd.Receipt.Type == "steer" && cmd.Receipt.Status == "accepted" {
				cmd.Receipt.Status = "rejected"
				rec.Commands[key] = cmd
				appendEvent(rec, "steering.rejected", cmd.Receipt.TurnID, key, map[string]any{"reason": "coordinator restarted before delivery confirmation"})
			}
		}
	}
	if err := s.persist(s.state); err != nil {
		return nil, errors.Join(err, held.Release())
	}
	go s.watchLeases()
	return s, nil
}

// Close stops owned executions and releases coordinator resources.
func (s *Service) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.stopSweep)
	for _, runs := range s.active {
		for _, run := range runs {
			run.cancel()
		}
	}
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return s.held.Release()
	case <-time.After(10 * time.Second):
		return errors.New("interactive executors have not stopped; coordinator lock retained")
	}
}
func id(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}
func digest(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (s *Service) clean(text string) string { return string(s.scrubber.Scrub([]byte(text))) }
func validCommandID(id string) bool {
	return len(id) > 0 && len(id) <= 128 && strings.TrimSpace(id) == id
}
func (s *Service) surface(id string) (Surface, bool) {
	for _, surface := range s.opts.Surfaces {
		if surface.ID == id {
			return surface, true
		}
	}
	return Surface{}, false
}
func (s *Service) record(owner, id string) (*sessionRecord, error) {
	rec := s.state.Sessions[id]
	if rec == nil || rec.Session.Owner != owner {
		return nil, problem(404, "not_found", "session is unavailable")
	}
	return rec, nil
}

// Session returns the session visible to its owner.
func (s *Service) Session(owner, id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.record(owner, id)
	if err != nil {
		return Session{}, err
	}
	return clone(rec.Session), nil
}

// ListSessions lists sessions belonging to an owner.
func (s *Service) ListSessions(owner string) []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sessions := []Session{}
	for _, rec := range s.state.Sessions {
		if rec.Session.Owner == owner {
			sessions = append(sessions, clone(rec.Session))
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].UpdatedAt.After(sessions[j].UpdatedAt) })
	return sessions
}

// Create creates or replays an idempotent session request.
func (s *Service) Create(owner string, req CreateRequest) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.reloading {
		return Session{}, problem(503, "unavailable", "coordinator is closed")
	}
	if owner == "" || !validCommandID(req.CommandID) || len(req.Title) > 200 {
		return Session{}, problem(400, "invalid", "owner, commandId and bounded title required")
	}
	key := owner + "\x00" + req.CommandID
	hash := digest(req)
	if old, ok := s.state.Creates[key]; ok {
		if old.Digest != hash {
			return Session{}, problem(409, "idempotency_conflict", "commandId was used with different content")
		}
		return clone(s.state.Sessions[old.SessionID].Session), nil
	}
	surface, ok := s.surface(req.SurfaceID)
	if !ok || !surface.Enabled {
		return Session{}, problem(404, "surface_unavailable", "configured surface is unavailable")
	}
	executionMode := req.ExecutionMode
	if executionMode == "" {
		executionMode = surface.DefaultExecutionMode
	}
	if executionMode == "" {
		executionMode = s.opts.DefaultExecutionMode
	}
	if !s.executionModeAvailable(executionMode) || !surfaceAllowsMode(surface, executionMode) {
		return Session{}, problem(409, "execution_mode_unavailable", "requested execution mode is not enabled by this surface and host")
	}
	if len(s.state.Sessions) >= 1000 {
		return Session{}, problem(409, "capacity", "prototype session retention limit reached")
	}
	now := time.Now().UTC()
	title := s.clean(strings.TrimSpace(req.Title))
	if title == "" {
		title = "New " + surface.Name + " session"
	}
	session := Session{ConfigRevision: surface.ConfigRevision, ExecutionMode: executionMode, ConfigDigest: digest(surface), Members: []Member{{Principal: owner, Role: "owner"}}, ID: id("ses_"), Owner: owner, SurfaceID: surface.ID, Mode: surface.Mode, Title: title, Status: "idle", Fence: 1, CreatedAt: now, UpdatedAt: now, Turns: []Turn{}, Children: []Child{}, Changes: []Change{}}
	err := s.mutate(func(state *diskState) error {
		rec := &sessionRecord{Session: session, Events: []Event{}, Commands: map[string]storedCommand{}}
		appendEvent(rec, "session.created", "", req.CommandID, map[string]any{"mode": surface.Mode, "surfaceId": surface.ID, "executionMode": executionMode})
		state.Sessions[session.ID] = rec
		state.Creates[key] = storedCreate{hash, session.ID}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	return clone(s.state.Sessions[session.ID].Session), nil
}

type commandOrigin struct {
	source string
	turnID string
	digest string
}

// Command validates and records a session command before dispatching its effects.
func (s *Service) Command(ctx context.Context, owner, sessionID string, cmd Command) (CommandReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if isDriverControl(cmd.Type) {
		return s.driverControlLocked(owner, sessionID, cmd)
	}
	return s.commandLocked(ctx, owner, sessionID, cmd, commandOrigin{source: "owner"})
}

// commandLocked holds the same coordinator lock as admission/replay. Stop
// temporarily releases it only around broker/custody callbacks.
func (s *Service) commandLocked(ctx context.Context, owner, sessionID string, cmd Command, origin commandOrigin) (CommandReceipt, error) {
	if s.closed {
		return CommandReceipt{}, problem(503, "unavailable", "coordinator is closed")
	}
	rec, err := s.record(owner, sessionID)
	if err != nil {
		return CommandReceipt{}, err
	}
	if !validCommandID(cmd.CommandID) || len(cmd.Text) > 32768 {
		return CommandReceipt{}, problem(400, "invalid", "commandId and text size must be valid")
	}
	if len(cmd.WorkflowYAML) > 32<<10 {
		return CommandReceipt{}, problem(400, "invalid_workflow", "child workflow definition exceeds 32 KiB")
	}
	if cmd.WorkflowYAML != "" && cmd.Type != "child.request" {
		return CommandReceipt{}, problem(400, "invalid_workflow", "workflow definition is immutable and may only be supplied when proposing a child")
	}
	if cmd.WorkflowYAML != "" && s.clean(cmd.WorkflowYAML) != cmd.WorkflowYAML {
		return CommandReceipt{}, problem(400, "secret_in_workflow", "child workflow definition contains credential-shaped material")
	}
	hash := digest(cmd)
	if origin.digest != "" {
		hash = origin.digest
	}
	if old, ok := rec.Commands[cmd.CommandID]; ok {
		if old.Digest != hash {
			return CommandReceipt{}, problem(409, "idempotency_conflict", "commandId was used with different content")
		}
		return old.Receipt, nil
	}
	if rec.Session.Sequence != cmd.ExpectedSequence {
		return CommandReceipt{}, problem(409, "stale_sequence", "refresh session before submitting this command")
	}
	if rec.Session.Fence != cmd.ExpectedFence {
		return CommandReceipt{}, problem(409, "stale_fence", "execution fence changed")
	}
	surface, exists := s.surface(rec.Session.SurfaceID)
	reviewOnly := cmd.Type == "child.review" && (cmd.Decision == "deny" || cmd.Decision == "accept")
	if s.reloading && cmd.Type != "stop" && !reviewOnly {
		return CommandReceipt{}, problem(503, "policy_reloading", "surface policy reload is in progress")
	}
	if cmd.Type != "stop" && !reviewOnly && (!s.executionModeAvailable(rec.Session.ExecutionMode) || !surfaceAllowsMode(surface, rec.Session.ExecutionMode)) {
		return CommandReceipt{}, problem(409, "execution_mode_unavailable", "session execution mode is unpinned or unavailable; create a new session with an enabled mode")
	}

	if cmd.Type != "stop" && !reviewOnly && rec.Session.ConfigDigest != digest(surface) {
		return CommandReceipt{}, problem(409, "surface_changed", "surface policy changed; create a new session for the new configuration")
	}
	if cmd.Type != "stop" && !reviewOnly && (!exists || !surface.Enabled) {
		return CommandReceipt{}, problem(409, "surface_unavailable", "surface is disabled; stop remains available")
	}
	if cmd.Type != "stop" && !reviewOnly && rec.Session.StopFencePending {
		return CommandReceipt{}, problem(409, "invalid_state", "external authority fence is pending; retry stop before any new work")
	}
	if cmd.Type == "resume" && len(s.active[sessionID]) > 0 {
		return CommandReceipt{}, problem(409, "recovery_required", "prior writer is still stopping")
	}
	if len(rec.Commands) >= 10000 {
		return CommandReceipt{}, problem(409, "capacity", "prototype command retention limit reached")
	}
	receipt := CommandReceipt{CommandID: cmd.CommandID, SessionID: sessionID, Type: cmd.Type, Status: "accepted"}
	var launch *ExecutionRequest
	var steerRun *activeRun
	var stop bool
	var custody []ExecutionRequest
	text := s.clean(strings.TrimSpace(cmd.Text))
	switch cmd.Type {
	case "message":
		if len(s.active[sessionID]) > 0 && rec.Session.Status == "idle" {
			return receipt, problem(409, "checkpoint_required", "wait for child snapshot work to finish before editing the parent")
		}
		if text == "" {
			return receipt, problem(400, "invalid", "message text is required")
		}
		if rec.Session.Status != "idle" && rec.Session.Status != "error" {
			return receipt, problem(409, "busy", "session must be idle before a new message")
		}
		if s.opts.Executor == nil {
			return receipt, problem(503, "executor_unavailable", "no interactive executor is configured")
		}
		if rec.Session.OutputBytes >= s.opts.MaxSessionOutputBytes {
			return receipt, problem(409, "capacity", "session output retention budget reached; create a new session")
		}
		if len(rec.Session.Turns) >= 1000 {
			return receipt, problem(409, "capacity", "prototype turn retention limit reached")
		}
		turnID := id("turn_")
		receipt.TurnID = turnID
		launch = &ExecutionRequest{SessionID: sessionID, TurnID: turnID, Owner: owner, SurfaceID: surface.ID, Mode: surface.Mode, ExecutionMode: rec.Session.ExecutionMode, Text: text, WorkspacePath: rec.Session.WorkspacePath, Fence: rec.Session.Fence, Attempt: 1, History: clone(rec.Session.Turns), Instructions: surface.Instructions}
	case "steer":
		if err := validateSteeringCommand(cmd, text, rec.Session); err != nil {
			return receipt, err
		}
		steerRun = s.active[sessionID][cmd.TurnID]
		if steerRun == nil {
			return receipt, problem(409, "stale_turn", "target attempt is unavailable")
		}
		if len(steerRun.steering) >= cap(steerRun.steering) {
			return receipt, problem(409, "capacity", "steering queue is full")
		}
		receipt.TurnID = cmd.TurnID
	case "stop":
		if len(s.active[sessionID]) == 0 && (rec.Session.Status == "recovery_required" || rec.Session.CustodyPending) {
			base := ExecutionRequest{SessionID: sessionID, Owner: owner, SurfaceID: rec.Session.SurfaceID, Mode: rec.Session.Mode, ExecutionMode: rec.Session.ExecutionMode, TurnID: rec.Session.ActiveTurnID, WorkspacePath: rec.Session.WorkspacePath, Fence: rec.Session.Fence}
			custody = append(custody, base)
			for _, child := range rec.Session.Children {
				if child.Status == "uncertain" {
					next := base
					next.ChildID = child.ID
					next.ReadOnly = true
					custody = append(custody, next)
				}
			}
		}
		stop = true
		receipt.TurnID = rec.Session.ActiveTurnID
	case "resume":
		if rec.Session.StopRequested {
			return receipt, problem(409, "invalid_state", "stop was requested; retry stop to verify remaining custody")
		}
		if rec.Session.Status != "recovery_required" && rec.Session.Status != "waiting_auth" {
			return receipt, problem(409, "invalid_state", "only waiting or interrupted sessions can resume")
		}
		if s.opts.Executor == nil {
			return receipt, problem(503, "executor_unavailable", "no interactive executor is configured")
		}
		if rec.Session.ActiveTurnID == "" {
			return receipt, problem(409, "recovery_required", "interrupted child requires operator reconciliation; no parent turn is resumable")
		}
		var turn *Turn
		for i := range rec.Session.Turns {
			if rec.Session.Turns[i].ID == rec.Session.ActiveTurnID {
				turn = &rec.Session.Turns[i]
			}
		}
		if turn == nil {
			return receipt, problem(409, "recovery_required", "interrupted turn is unavailable")
		}
		request := ExecutionRequest{SessionID: sessionID, TurnID: turn.ID, Owner: owner, SurfaceID: surface.ID, Mode: surface.Mode, ExecutionMode: rec.Session.ExecutionMode, Text: turn.Text, WorkspacePath: rec.Session.WorkspacePath, Fence: rec.Session.Fence + 1, Attempt: turn.Attempt + 1, History: clone(rec.Session.Turns), Instructions: surface.Instructions}
		s.bindSurfaceRequest(&request, surface)
		if rec.Session.Status == "recovery_required" {
			recovery, ok := s.opts.Executor.(RecoveryExecutor)
			if !ok {
				return receipt, problem(409, "recovery_required", "executor cannot verify prior writer custody")
			}
			if err := recovery.Recover(ctx, request); err != nil {
				return receipt, problem(409, "recovery_required", s.clean(err.Error()))
			}
			for _, child := range rec.Session.Children {
				if child.Status == "uncertain" {
					childRequest := request
					childRequest.ChildID = child.ID
					childRequest.TurnID = child.ID
					childRequest.ReadOnly = true
					s.bindSurfaceRequest(&childRequest, surface)
					if err := recovery.Recover(ctx, childRequest); err != nil {
						return receipt, problem(409, "recovery_required", s.clean(err.Error()))
					}
				}
			}
		}
		launch = &request
		receipt.TurnID = turn.ID
	case "child.request":
		if text == "" {
			return receipt, problem(400, "invalid", "child prompt is required")
		}
		if rec.Session.Status != "idle" && rec.Session.Status != "running" {
			return receipt, problem(409, "invalid_state", "child cannot be requested in the current state")
		}
		if len(rec.Session.Children) >= surface.MaxChildren {
			return receipt, problem(409, "child_limit", "surface child budget is exhausted")
		}
		receipt.ChildID = id("child_")
		receipt.TurnID = origin.turnID
	case "child.review":
		var child *Child
		for i := range rec.Session.Children {
			if rec.Session.Children[i].ID == cmd.ChildID {
				child = &rec.Session.Children[i]
			}
		}
		if child == nil {
			return receipt, problem(404, "not_found", "child is unavailable")
		}
		receipt.ChildID = child.ID
		switch cmd.Decision {
		case "approve":
			if rec.Session.Status != "idle" {
				return receipt, problem(409, "checkpoint_required", "wait for the parent turn to finish before approving a child snapshot")
			}
			if child.Status != "proposed" {
				return receipt, problem(409, "invalid_state", "child is not eligible for approval")
			}
			if s.opts.Executor == nil {
				return receipt, problem(503, "executor_unavailable", "no interactive executor is configured")
			}
			receipt.TurnID = child.ID
			launch = &ExecutionRequest{SessionID: sessionID, TurnID: child.ID, ChildID: child.ID, Owner: owner, SurfaceID: surface.ID, Mode: surface.Mode, ExecutionMode: rec.Session.ExecutionMode, WorkflowYAML: child.WorkflowYAML, Text: child.Prompt, WorkspacePath: rec.Session.WorkspacePath, Fence: rec.Session.Fence, Attempt: 1, History: clone(rec.Session.Turns), Instructions: surface.Instructions, ReadOnly: true}
		case "deny":
			if child.Status != "proposed" {
				return receipt, problem(409, "invalid_state", "only proposed children can be denied")
			}
		case "accept":
			if child.Status != "succeeded" && child.Status != "failed" {
				return receipt, problem(409, "invalid_state", "only completed child results can be reviewed")
			}
		default:
			return receipt, problem(400, "invalid", "child decision must be approve, deny or accept")
		}
	default:
		return receipt, problem(400, "invalid", "unknown interactive command type")
	}
	if launch != nil {
		s.bindSurfaceRequest(launch, surface)
		if err := s.checkCapacity(surface, owner, sessionID, cmd.Type == "resume"); err != nil {
			return receipt, err
		}

		launch.WorkerID = s.opts.WorkerID
		launch.LeaseID = id("lease_")
	}
	err = s.mutate(func(state *diskState) error {
		current := state.Sessions[sessionID]
		firstEvent := len(current.Events)
		appendEvent(current, "command.accepted", receipt.TurnID, cmd.CommandID, map[string]any{"commandType": cmd.Type, "text": text})
		switch cmd.Type {
		case "message":
			current.Session.Lease = s.newLease(*launch)
			current.Session.Status = "running"
			current.Session.ActiveTurnID = launch.TurnID
			current.Session.Turns = append(current.Session.Turns, Turn{ID: launch.TurnID, Text: text, Status: "running", Attempt: 1, Fence: launch.Fence, StartedAt: time.Now().UTC()})
			appendEvent(current, "turn.started", launch.TurnID, cmd.CommandID, map[string]any{"attempt": 1, "fence": launch.Fence})
		case "resume":
			current.Session.CustodyPending = false
			for i := range current.Session.Children {
				if current.Session.Children[i].Lease != nil && current.Session.Children[i].Lease.Status == "custody_pending" {
					current.Session.Children[i].Lease.Status = "released"
				}
			}
			current.Session.Lease = s.newLease(*launch)
			current.Session.Status = "running"
			current.Session.Fence = launch.Fence
			current.Session.RecoveryReason = ""
			for i := range current.Session.Turns {
				if current.Session.Turns[i].ID == launch.TurnID {
					current.Session.Turns[i].Status = "running"
					current.Session.Turns[i].Attempt = launch.Attempt
					current.Session.Turns[i].Fence = launch.Fence
					current.Session.Turns[i].FinishedAt = nil
					current.Session.Turns[i].Error = ""
				}
			}
			appendEvent(current, "turn.started", launch.TurnID, cmd.CommandID, map[string]any{"attempt": launch.Attempt, "fence": launch.Fence, "resumed": true})
		case "stop":
			current.Session.StopRequested = true
			current.Session.Fence++
			current.Session.StopFencePending = s.opts.OnStop != nil
			current.Session.CustodyPending = current.Session.CustodyPending || len(custody) > 0
			current.Session.Status = "stopping"
			appendEvent(current, "session.stopping", current.Session.ActiveTurnID, cmd.CommandID, map[string]any{"fence": current.Session.Fence})
			for i := range current.Session.Children {
				if current.Session.Children[i].Status == "proposed" {
					current.Session.Children[i].Status = "cancelled"
				}
			}
			if len(s.active[sessionID]) == 0 && !current.Session.StopFencePending && !current.Session.CustodyPending {
				markStopped(current)
			}
		case "child.request":
			executionKind := "interactive-review"
			if cmd.WorkflowYAML != "" {
				executionKind = "interactive-workflow"
			}
			current.Session.Children = append(current.Session.Children, Child{WorkflowYAML: cmd.WorkflowYAML, WorkflowDigest: workflowDigest(cmd.WorkflowYAML), ActorSource: origin.source, SourceTurnID: origin.turnID, ExecutionKind: executionKind, ReadOnly: true, ID: receipt.ChildID, Prompt: text, Status: "proposed", RequestedAt: time.Now().UTC()})
			appendEvent(current, "child.proposed", receipt.TurnID, cmd.CommandID, map[string]any{"childId": receipt.ChildID, "prompt": text, "readOnly": true, "workflowYaml": cmd.WorkflowYAML, "workflowDigest": workflowDigest(cmd.WorkflowYAML), "sourceTurnId": origin.turnID})
		case "child.review":
			for i := range current.Session.Children {
				child := &current.Session.Children[i]
				if child.ID != cmd.ChildID {
					continue
				}
				now := time.Now().UTC()
				child.ReviewedAt = &now
				switch cmd.Decision {
				case "approve":
					child.Status = "running"
					child.Lease = s.newLease(*launch)
				case "deny":
					child.Status = "denied"
				case "accept":
					child.Status = "reviewed"
				}
			}
			kind := "child.reviewed"
			if cmd.Decision == "approve" {
				kind = "child.started"
			}
			appendEvent(current, kind, receipt.TurnID, cmd.CommandID, map[string]any{"childId": cmd.ChildID, "decision": cmd.Decision})
		}
		for i := firstEvent; i < len(current.Events); i++ {
			current.Events[i].ActorSource = origin.source
		}
		receipt.Sequence = current.Session.Sequence
		current.Commands[cmd.CommandID] = storedCommand{hash, receipt}
		return nil
	})
	if err != nil {
		return CommandReceipt{}, err
	}
	if launch != nil {
		s.start(*launch)
	}
	if steerRun != nil {
		steerRun.steering <- s.steeringMessage(sessionID, cmd, text)
	}
	if stop && len(custody) > 0 {
		s.mu.Unlock()
		var custodyErr error
		recoverer, ok := s.opts.Executor.(RecoveryExecutor)
		if !ok {
			custodyErr = errors.New("executor cannot verify prior writer custody")
		} else {
			for _, request := range custody {
				if err := recoverer.Recover(ctx, request); err != nil {
					custodyErr = err
					break
				}
			}
		}
		s.mu.Lock()
		err = s.mutate(func(state *diskState) error {
			current := state.Sessions[sessionID]
			current.Session.CustodyPending = custodyErr != nil
			if custodyErr != nil {
				current.Session.RecoveryReason = s.clean(custodyErr.Error())
				appendEvent(current, "recovery.required", "", cmd.CommandID, map[string]any{"reason": current.Session.RecoveryReason})
			}
			if custodyErr == nil {
				for i := range current.Session.Children {
					if current.Session.Children[i].Status == "uncertain" {
						current.Session.Children[i].Status = "cancelled"
					}
				}
			}
			if !current.Session.CustodyPending && !current.Session.StopFencePending && len(s.active[sessionID]) == 0 && current.Session.Status == "stopping" {
				markStopped(current)
			}
			return nil
		})
		if err != nil {
			return CommandReceipt{}, err
		}
	}
	if stop {
		// The session fence is already durable. Never hold the session lock
		// while acquiring the broker lock: broker admission checks us too.
		if s.opts.OnStop != nil {
			s.mu.Unlock()
			hookErr := s.opts.OnStop(ctx, owner, sessionID)
			s.mu.Lock()
			err = s.mutate(func(state *diskState) error {
				current := state.Sessions[sessionID]
				current.Session.StopFencePending = hookErr != nil
				if hookErr != nil {
					appendEvent(current, "auth.fence_pending", "", cmd.CommandID, map[string]any{"reason": s.clean(hookErr.Error())})
				} else if len(s.active[sessionID]) == 0 && !current.Session.CustodyPending && current.Session.Status == "stopping" {
					markStopped(current)
				}
				return nil
			})
			if err != nil {
				return CommandReceipt{}, err
			}
		}
		for _, run := range s.active[sessionID] {
			run.cancel()
		}
	}
	return receipt, nil
}

func markStopped(rec *sessionRecord) {
	rec.Session.Status = "stopped"
	if rec.Session.Lease != nil {
		rec.Session.Lease.Status = "released"
	}
	for i := range rec.Session.Children {
		if rec.Session.Children[i].Lease != nil && rec.Session.Children[i].Status == "cancelled" {
			rec.Session.Children[i].Lease.Status = "released"
		}
	}
	now := time.Now().UTC()
	for i := range rec.Session.Turns {
		if rec.Session.Turns[i].Status == "running" || rec.Session.Turns[i].Status == "interrupted" || rec.Session.Turns[i].Status == "waiting_auth" {
			rec.Session.Turns[i].Status = "cancelled"
			rec.Session.Turns[i].FinishedAt = &now
		}
	}
	rec.Session.ActiveTurnID = ""
	appendEvent(rec, "session.stopped", "", "", map[string]any{"writersStopped": true})
}

func (s *Service) ackSteering(sessionID, commandID, turnID string, fence uint64, delivery, steeringMode string, deliveryErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.mutate(func(state *diskState) error {
		rec := state.Sessions[sessionID]
		cmd, ok := rec.Commands[commandID]
		if !ok || cmd.Receipt.Status != "accepted" {
			return nil
		}
		if rec.Session.Status != "running" || rec.Session.ActiveTurnID != turnID || rec.Session.Fence != fence {
			deliveryErr = errors.New("target attempt ended before delivery was acknowledged")
		}
		kind := "steering.delivered"
		cmd.Receipt.Status = "delivered"
		data := map[string]any{}
		if deliveryErr == nil && delivery != "" {
			cmd.Receipt.Delivery = delivery
			cmd.Receipt.SteeringMode = steeringMode
			data["delivery"] = delivery
			if steeringMode != "" {
				data["steeringMode"] = steeringMode
			}
		}
		if deliveryErr != nil {
			kind = "steering.rejected"
			cmd.Receipt.Status = "rejected"
			data["reason"] = s.clean(deliveryErr.Error())
		}
		appendEvent(rec, kind, turnID, commandID, data)
		rec.Commands[commandID] = cmd
		return nil
	})
}

// CheckControl admits a helper control-plane action against the currently
// configured surface and private session. It does not authorize a provider
// operation, which must additionally pass its own grant and lease checks.
// History and artifacts remain readable without this admission check.
func (s *Service) CheckControl(owner, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkControlLocked(owner, sessionID)
}

func (s *Service) checkControlLocked(owner, sessionID string) error {
	if s.closed || s.fatal != nil || s.reloading {
		return problem(503, "unavailable", "interactive coordinator is unavailable")
	}
	rec, err := s.record(owner, sessionID)
	if err != nil {
		return err
	}
	surface, ok := s.surface(rec.Session.SurfaceID)
	if !ok || !surface.Enabled {
		return problem(409, "surface_unavailable", "configured surface is unavailable")
	}
	if rec.Session.ConfigDigest != digest(surface) {
		return problem(409, "surface_changed", "surface policy changed; create a new session for the new configuration")
	}
	if !s.executionModeAvailable(rec.Session.ExecutionMode) || !surfaceAllowsMode(surface, rec.Session.ExecutionMode) {
		return problem(409, "execution_mode_unavailable", "session execution mode is unpinned or unavailable; create a new session with an enabled mode")
	}

	if rec.Session.Status == "stopped" || rec.Session.Status == "stopping" || rec.Session.Status == "recovery_required" || rec.Session.CustodyPending || rec.Session.StopFencePending {
		return problem(409, "invalid_state", "session control is fenced or awaiting recovery")
	}
	return nil
}

// validateSteeringCommand validates delivery intent and its active-turn target.
func validateSteeringCommand(cmd Command, text string, session Session) error {
	if cmd.Delivery != "" && cmd.Delivery != "steer" && cmd.Delivery != "queued" && cmd.Delivery != "prompt" {
		return problem(400, "invalid", "unknown steering delivery mode")
	}
	if cmd.SteeringMode != "" && cmd.SteeringMode != "interrupt-and-continue" {
		return problem(400, "invalid", "unknown steering mode")
	}
	if (cmd.Delivery == "prompt") != (cmd.SteeringMode == "interrupt-and-continue") {
		return problem(400, "invalid", "prompt delivery requires interrupt-and-continue")
	}
	if session.ExecutionMode == "local-tools" && (cmd.Delivery == "queued" || cmd.SteeringMode != "") {
		return problem(409, "unsupported", "the demo supports immediate steering only")
	}
	if text == "" || cmd.TurnID == "" {
		return problem(400, "invalid", "steering text and exact turnId are required")
	}
	if session.Status != "running" || session.ActiveTurnID != cmd.TurnID {
		return problem(409, "stale_turn", "target turn is no longer active")
	}
	return nil
}

func (s *Service) steeringMessage(sessionID string, cmd Command, text string) Steering {
	var once sync.Once
	message := Steering{CommandID: cmd.CommandID, Text: text, Delivery: cmd.Delivery, SteeringMode: cmd.SteeringMode}
	message.Ack = func(err error) {
		once.Do(func() { s.ackSteering(sessionID, cmd.CommandID, cmd.TurnID, cmd.ExpectedFence, "", "", err) })
	}
	message.AckDelivery = func(delivery string, err error) {
		mode := ""
		if delivery == "prompt" {
			mode = "interrupt-and-continue"
		}
		once.Do(func() { s.ackSteering(sessionID, cmd.CommandID, cmd.TurnID, cmd.ExpectedFence, delivery, mode, err) })
	}
	return message
}
