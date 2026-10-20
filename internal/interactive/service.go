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
	if err := s.validateCommandPayload(cmd); err != nil {
		return CommandReceipt{}, err
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
	surface, err := s.commandSurface(sessionID, cmd, rec)
	if err != nil {
		return CommandReceipt{}, err
	}
	p := &preparedCommand{
		command: cmd, origin: origin,
		receipt: CommandReceipt{CommandID: cmd.CommandID, SessionID: sessionID, Type: cmd.Type, Status: "accepted"},
		text:    s.clean(strings.TrimSpace(cmd.Text)),
	}
	if err := s.prepareCommand(ctx, owner, rec, surface, p); err != nil {
		return p.receipt, err
	}

	if p.launch != nil {
		s.bindSurfaceRequest(p.launch, surface)
		if err := s.checkCapacity(surface, owner, p.receipt.SessionID, p.command.Type == "resume"); err != nil {
			return p.receipt, err
		}

		p.launch.WorkerID = s.opts.WorkerID
		p.launch.LeaseID = id("lease_")
	}
	err = s.commitCommand(p, hash)
	if err != nil {
		return CommandReceipt{}, err
	}
	if p.launch != nil {
		s.start(*p.launch)
	}
	if p.steerRun != nil {
		p.steerRun.steering <- s.steeringMessage(p.receipt.SessionID, p.command, p.text)
	}
	if p.command.Type == "stop" && len(p.custody) > 0 {
		if err := s.recoverStoppedCommand(ctx, p); err != nil {
			return CommandReceipt{}, err
		}
	}
	if p.command.Type == "stop" {
		if err := s.stopCommand(ctx, owner, p); err != nil {
			return CommandReceipt{}, err
		}
	}
	return p.receipt, nil
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
