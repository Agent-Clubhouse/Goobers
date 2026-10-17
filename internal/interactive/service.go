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
}
type Service struct {
	mu        sync.Mutex
	opts      Options
	state     diskState
	active    map[string]map[string]*activeRun
	changed   chan struct{}
	held      *lock.Handle
	scrubber  *secretpattern.Scrubber
	wg        sync.WaitGroup
	closed    bool
	stopSweep chan struct{}
	fatal     error
}

func Open(opts Options) (*Service, error) {
	if opts.StateDir == "" {
		return nil, errors.New("interactive StateDir is required")
	}
	if opts.Principal == nil {
		return nil, errors.New("interactive Principal resolver is required")
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
	opts.Surfaces = clone(opts.Surfaces)
	scrubber := secretpattern.NewScrubber()
	for i := range opts.Surfaces {
		opts.Surfaces[i].Instructions = string(scrubber.Scrub([]byte(opts.Surfaces[i].Instructions)))
	}
	seen := map[string]bool{}
	for _, surface := range opts.Surfaces {
		if surface.ID == "" || seen[surface.ID] || (surface.Mode != "helper" && surface.Mode != "workbench") || surface.MaxChildren < 0 || surface.MaxChildren > 8 {
			return nil, errors.New("invalid or duplicate interactive surface")
		}
		seen[surface.ID] = true
	}
	if err := os.MkdirAll(opts.StateDir, 0700); err != nil {
		return nil, err
	}
	held, err := lock.TryAcquire(filepath.Join(opts.StateDir, "sessions.lock"))
	if err != nil {
		return nil, fmt.Errorf("interactive coordinator already owned or inaccessible: %w", err)
	}
	s := &Service{opts: opts, state: emptyState(), active: map[string]map[string]*activeRun{}, changed: make(chan struct{}), held: held, scrubber: secretpattern.NewScrubber(), stopSweep: make(chan struct{})}
	b, err := os.ReadFile(filepath.Join(opts.StateDir, "sessions.json"))
	if err == nil {
		err = json.Unmarshal(b, &s.state)
	} else if os.IsNotExist(err) {
		err = nil
	}
	if err != nil || s.state.Version != 1 || s.state.Sessions == nil || s.state.Creates == nil {
		held.Release()
		return nil, fmt.Errorf("invalid interactive state: %w", err)
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
		held.Release()
		return nil, err
	}
	go s.watchLeases()
	return s, nil
}

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
func (s *Service) Session(owner, id string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.record(owner, id)
	if err != nil {
		return Session{}, err
	}
	return clone(rec.Session), nil
}
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
func (s *Service) Create(owner string, req CreateRequest) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
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
	if len(s.state.Sessions) >= 1000 {
		return Session{}, problem(409, "capacity", "prototype session retention limit reached")
	}
	now := time.Now().UTC()
	title := s.clean(strings.TrimSpace(req.Title))
	if title == "" {
		title = "New " + surface.Name + " session"
	}
	session := Session{ConfigDigest: digest(surface), Members: []Member{{Principal: owner, Role: "owner"}}, ID: id("ses_"), Owner: owner, SurfaceID: surface.ID, Mode: surface.Mode, Title: title, Status: "idle", Fence: 1, CreatedAt: now, UpdatedAt: now, Turns: []Turn{}, Children: []Child{}, Changes: []Change{}}
	err := s.mutate(func(state *diskState) error {
		rec := &sessionRecord{Session: session, Events: []Event{}, Commands: map[string]storedCommand{}}
		appendEvent(rec, "session.created", "", req.CommandID, map[string]any{"mode": surface.Mode, "surfaceId": surface.ID})
		state.Sessions[session.ID] = rec
		state.Creates[key] = storedCreate{hash, session.ID}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	return clone(s.state.Sessions[session.ID].Session), nil
}

func (s *Service) Command(ctx context.Context, owner, sessionID string, cmd Command) (CommandReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	hash := digest(cmd)
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
	if cmd.Type != "stop" && !reviewOnly && rec.Session.ConfigDigest != digest(surface) {
		return CommandReceipt{}, problem(409, "surface_changed", "surface policy changed; create a new session for the new configuration")
	}
	if cmd.Type != "stop" && !reviewOnly && (!exists || !surface.Enabled) {
		return CommandReceipt{}, problem(409, "surface_unavailable", "surface is disabled; stop remains available")
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
		launch = &ExecutionRequest{SessionID: sessionID, TurnID: turnID, Owner: owner, SurfaceID: surface.ID, Mode: surface.Mode, Text: text, WorkspacePath: rec.Session.WorkspacePath, Fence: rec.Session.Fence, Attempt: 1, History: clone(rec.Session.Turns), Instructions: surface.Instructions}
	case "steer":
		if text == "" || cmd.TurnID == "" {
			return receipt, problem(400, "invalid", "steering text and exact turnId are required")
		}
		if rec.Session.Status != "running" || rec.Session.ActiveTurnID != cmd.TurnID {
			return receipt, problem(409, "stale_turn", "target turn is no longer active")
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
			base := ExecutionRequest{SessionID: sessionID, Owner: owner, SurfaceID: rec.Session.SurfaceID, Mode: rec.Session.Mode, TurnID: rec.Session.ActiveTurnID, WorkspacePath: rec.Session.WorkspacePath, Fence: rec.Session.Fence}
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
		request := ExecutionRequest{SessionID: sessionID, TurnID: turn.ID, Owner: owner, SurfaceID: surface.ID, Mode: surface.Mode, Text: turn.Text, WorkspacePath: rec.Session.WorkspacePath, Fence: rec.Session.Fence + 1, Attempt: turn.Attempt + 1, History: clone(rec.Session.Turns), Instructions: surface.Instructions}
		if rec.Session.Status == "recovery_required" {
			recovery, ok := s.opts.Executor.(RecoveryExecutor)
			if !ok {
				return receipt, problem(409, "recovery_required", "executor cannot verify prior writer custody")
			}
			if err := recovery.Recover(ctx, request); err != nil {
				return receipt, problem(409, "recovery_required", s.clean(err.Error()))
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
			if child.Status != "proposed" || (rec.Session.Status != "idle" && rec.Session.Status != "running") {
				return receipt, problem(409, "invalid_state", "child is not eligible for approval")
			}
			if s.opts.Executor == nil {
				return receipt, problem(503, "executor_unavailable", "no interactive executor is configured")
			}
			launch = &ExecutionRequest{SessionID: sessionID, TurnID: rec.Session.ActiveTurnID, ChildID: child.ID, Owner: owner, SurfaceID: surface.ID, Mode: surface.Mode, Text: child.Prompt, WorkspacePath: rec.Session.WorkspacePath, Fence: rec.Session.Fence, Attempt: 1, History: clone(rec.Session.Turns), Instructions: surface.Instructions, ReadOnly: true}
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
		total, owned := 0, 0
		for _, runs := range s.active {
			for _, run := range runs {
				total++
				if run.request.Owner == owner {
					owned++
				}
			}
		}
		if total >= s.opts.MaxActive || owned >= s.opts.MaxActivePerOwner {
			return receipt, problem(409, "capacity", "interactive execution budget is occupied; retry when active work finishes")
		}
		launch.WorkerID = s.opts.WorkerID
		launch.LeaseID = id("lease_")
	}
	err = s.mutate(func(state *diskState) error {
		current := state.Sessions[sessionID]
		appendEvent(current, "command.accepted", receipt.TurnID, cmd.CommandID, map[string]any{"commandType": cmd.Type, "text": text})
		switch cmd.Type {
		case "message":
			current.Session.Lease = s.newLease(*launch)
			current.Session.Status = "running"
			current.Session.ActiveTurnID = launch.TurnID
			current.Session.Turns = append(current.Session.Turns, Turn{ID: launch.TurnID, Text: text, Status: "running", Attempt: 1, Fence: launch.Fence, StartedAt: time.Now().UTC()})
			appendEvent(current, "turn.started", launch.TurnID, cmd.CommandID, map[string]any{"attempt": 1, "fence": launch.Fence})
		case "resume":
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
			current.Session.Fence++
			current.Session.StopFencePending = s.opts.OnStop != nil
			current.Session.CustodyPending = len(custody) > 0
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
			current.Session.Children = append(current.Session.Children, Child{ExecutionKind: "interactive-review", ReadOnly: true, ID: receipt.ChildID, Prompt: text, Status: "proposed", RequestedAt: time.Now().UTC()})
			appendEvent(current, "child.proposed", "", cmd.CommandID, map[string]any{"childId": receipt.ChildID, "prompt": text, "readOnly": true})
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
			appendEvent(current, kind, "", cmd.CommandID, map[string]any{"childId": cmd.ChildID, "decision": cmd.Decision})
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
		var once sync.Once
		message := Steering{CommandID: cmd.CommandID, Text: text}
		message.Ack = func(err error) {
			once.Do(func() { s.ackSteering(sessionID, cmd.CommandID, cmd.TurnID, cmd.ExpectedFence, err) })
		}
		steerRun.steering <- message
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

func (s *Service) ackSteering(sessionID, commandID, turnID string, fence uint64, deliveryErr error) {
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
