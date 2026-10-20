package interactive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

func (s *Service) newLease(req ExecutionRequest) *Lease {
	return &Lease{WorkerID: req.WorkerID, LeaseID: req.LeaseID, Fence: req.Fence, ExpiresAt: s.leaseExpiry(req), Status: "active"}
}
func (s *Service) leaseExpiry(req ExecutionRequest) time.Time {
	duration := req.leaseDuration
	if duration <= 0 {
		duration = s.opts.LeaseDuration
	}
	expiry := s.opts.Now().Add(duration)
	if !req.turnExpiresAt.IsZero() && req.turnExpiresAt.Before(expiry) {
		expiry = req.turnExpiresAt
	}
	return expiry
}
func runKey(req ExecutionRequest) string {
	if req.ChildID != "" {
		return req.ChildID
	}
	return req.TurnID
}
func leaseFor(rec *sessionRecord, childID string) *Lease {
	if childID == "" {
		return rec.Session.Lease
	}
	for i := range rec.Session.Children {
		if rec.Session.Children[i].ID == childID {
			return rec.Session.Children[i].Lease
		}
	}
	return nil
}
func (s *Service) heartbeat(req ExecutionRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.state.Sessions[req.SessionID]
	lease := leaseFor(rec, req.ChildID)
	if s.closed || rec.Session.Fence != req.Fence || lease == nil || lease.LeaseID != req.LeaseID || lease.Status != "active" || !lease.ExpiresAt.After(s.opts.Now()) {
		return problem(409, "stale_lease", "worker lease expired or was fenced")
	}
	return s.mutate(func(state *diskState) error {
		leaseFor(state.Sessions[req.SessionID], req.ChildID).ExpiresAt = s.leaseExpiry(req)
		return nil
	})
}
func (s *Service) leaseSweepInterval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	duration := s.opts.LeaseDuration
	for _, surface := range s.opts.Surfaces {
		duration = boundedDuration(surface.LeaseSeconds, duration)
	}
	interval := duration / 4
	if interval > time.Second {
		interval = time.Second
	}
	if interval < 10*time.Millisecond {
		interval = 10 * time.Millisecond
	}
	return interval
}
func (s *Service) watchLeases() {
	ticker := time.NewTicker(s.leaseSweepInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.SweepLeases()
		case <-s.policyWake:
			ticker.Reset(s.leaseSweepInterval())
		case <-s.stopSweep:
			return
		}
	}
}

// SweepLeases is safe to invoke from an embedded scheduler. Tests can advance
// Options.Now before calling it. It is deliberately not an HTTP debug endpoint.
func (s *Service) SweepLeases() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	expired := map[string]bool{}
	for sessionID, runs := range s.active {
		rec := s.state.Sessions[sessionID]
		for _, run := range runs {
			lease := leaseFor(rec, run.request.ChildID)
			if lease != nil && lease.Status == "active" && !lease.ExpiresAt.After(s.opts.Now()) {
				expired[sessionID] = true
			}
		}
	}
	if len(expired) == 0 {
		return
	}
	err := s.mutate(func(state *diskState) error {
		for sessionID := range expired {
			rec := state.Sessions[sessionID]
			rec.Session.Fence++
			rec.Session.Status = "recovery_required"
			rec.Session.RecoveryReason = "Worker lease expired; prior writer must stop before explicit recovery."
			if rec.Session.Lease != nil && rec.Session.Lease.Status == "active" {
				rec.Session.Lease.Status = "expired"
			}
			for i := range rec.Session.Children {
				if rec.Session.Children[i].Status == "running" {
					rec.Session.Children[i].Status = "uncertain"
					if rec.Session.Children[i].Lease != nil {
						rec.Session.Children[i].Lease.Status = "expired"
					}
				}
			}
			for i := range rec.Session.Turns {
				if rec.Session.Turns[i].Status == "running" {
					rec.Session.Turns[i].Status = "interrupted"
				}
			}
			appendEvent(rec, "lease.expired", rec.Session.ActiveTurnID, "", map[string]any{"fence": rec.Session.Fence, "reason": rec.Session.RecoveryReason})
		}
		return nil
	})
	if err == nil {
		for sessionID := range expired {
			for _, run := range s.active[sessionID] {
				run.cancel()
			}
		}
	}
}

func (s *Service) start(req ExecutionRequest) {
	duration := req.maxTurnDuration
	if duration <= 0 {
		duration = s.opts.MaxTurnDuration
	}
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	steering := make(chan Steering, 16)
	req.Steering = steering
	controls := make(chan DriverControl, 16)
	req.Controls = controls
	req.Heartbeat = func() error { return s.heartbeat(req) }
	run := &activeRun{cancel: cancel, request: req, steering: steering, controls: controls}
	if s.active[req.SessionID] == nil {
		s.active[req.SessionID] = map[string]*activeRun{}
	}
	s.active[req.SessionID][runKey(req)] = run
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		// Preserve potentially split credential patterns across streamed deltas.
		var emitMu sync.Mutex
		pending := map[string]ExecutionEvent{}
		emit := func(event ExecutionEvent) error {
			emitMu.Lock()
			defer emitMu.Unlock()
			// Normalize typed tool results so nested structs cannot bypass
			// sensitive-key scrubbing or retain caller-owned mutable pointers.
			encoded, normalizeErr := json.Marshal(event.Data)
			if normalizeErr != nil {
				return problem(400, "invalid_event", "executor event must contain JSON-safe values")
			}
			var data map[string]any
			if err := json.Unmarshal(encoded, &data); err != nil {
				return problem(400, "invalid_event", "executor event must contain a JSON object")
			}
			if data == nil {
				data = map[string]any{}
			}
			event.Data = data
			if event.Type == "assistant.delta" || event.Type == "tool.output" {
				key := event.Type + "/" + fmt.Sprint(event.Data["name"]) + "/" + fmt.Sprint(event.Data["stream"]) + "/" + fmt.Sprint(event.Data["toolCallId"])
				previous := pending[key]
				before, _ := previous.Data["text"].(string)
				text, _ := event.Data["text"].(string)
				text = before + text
				if len(text) > 256<<10 {
					return errors.New("unsafe unfinished output exceeds streaming buffer")
				}
				boundary := s.scrubber.SafePrefix([]byte(text))
				retained := clone(event.Data)
				setStreamingText(retained, text[boundary:])
				pending[key] = ExecutionEvent{Type: event.Type, Data: retained}
				if boundary == 0 {
					return nil
				}
				event.Data = clone(event.Data)
				setStreamingText(event.Data, text[:boundary])
			}
			return s.emit(req, event)
		}
		var result ExecutionResult
		var err error
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					err = fmt.Errorf("executor panicked: %v", recovered)
				}
			}()
			result, err = s.opts.Executor.Execute(ctx, req, emit)
		}()
		emitMu.Lock()
		for _, event := range pending {
			if text, _ := event.Data["text"].(string); text != "" {
				_ = s.emit(req, event)
			}
		}
		emitMu.Unlock()
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		s.finish(req, result, err)
	}()
}

// Keep the v2 content in step with the safe prefix. Retaining the original
// chunk under data.chat would expose text withheld by the streaming scrubber.
func setStreamingText(data map[string]any, text string) {
	data["text"] = text
	if envelope, ok := data["chat"].(map[string]any); ok {
		if payload, ok := envelope["data"].(map[string]any); ok {
			payload["content"] = []any{map[string]any{"type": "text", "text": text}}
		}
	}
}

func (s *Service) cleanData(data map[string]any) map[string]any {
	result := map[string]any{}
	for key, value := range data {
		// Credentials must never be passed to this API. Drop explicit sensitive
		// fields as a second boundary in addition to broker custody and scrubbing.
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "authorization") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") {
			result[key] = "[REDACTED]"
			continue
		}
		switch value := value.(type) {
		case string:
			if len(value) > 128<<10 {
				value = value[:128<<10] + "\n[output truncated]"
			}
			result[key] = s.clean(value)
		case map[string]any:
			result[key] = s.cleanData(value)
		case []any:
			items := make([]any, len(value))
			for i, item := range value {
				items[i] = s.cleanData(map[string]any{"item": item})["item"]
			}
			result[key] = items
		default:
			result[key] = value
		}
	}
	return result
}
func (s *Service) emit(req ExecutionRequest, event ExecutionEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.state.Sessions[req.SessionID]
	lease := leaseFor(rec, req.ChildID)
	if rec.Session.Fence != req.Fence || lease == nil || lease.LeaseID != req.LeaseID || lease.Status != "active" || !lease.ExpiresAt.After(s.opts.Now()) {
		return problem(409, "stale_fence", "execution event belongs to an expired attempt")
	}
	switch event.Type {
	case "assistant.delta", "assistant.message", "tool.started", "tool.output", "tool.completed", "test.result", "context.loaded", "execution.note", "auth.required":
	default:
		return problem(400, "invalid_event", "executor cannot emit coordinator state transitions")
	}
	data := s.cleanData(event.Data)
	if req.ChildID != "" {
		data["childId"] = req.ChildID
	}
	dataBytes, _ := json.Marshal(data)
	if rec.Session.OutputBytes+len(dataBytes) > s.opts.MaxSessionOutputBytes {
		return problem(409, "capacity", "session output retention budget reached")
	}
	if len(rec.Events) > 20000 {
		return problem(409, "capacity", "prototype event retention limit reached")
	}
	return s.mutate(func(state *diskState) error {
		state.Sessions[req.SessionID].Session.OutputBytes += len(dataBytes)
		appendEvent(state.Sessions[req.SessionID], event.Type, req.TurnID, "", data)
		return nil
	})
}
func (s *Service) finish(req ExecutionRequest, result ExecutionResult, runErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active[req.SessionID], runKey(req))
	if len(s.active[req.SessionID]) == 0 {
		delete(s.active, req.SessionID)
	}
	_ = s.mutate(func(state *diskState) error {
		rec := state.Sessions[req.SessionID]
		current := rec.Session.Fence == req.Fence
		lease := leaseFor(rec, req.ChildID)
		wasPolicyFenced := lease != nil && lease.LeaseID == req.LeaseID && lease.Status == "policy_fenced"
		if lease != nil && lease.LeaseID == req.LeaseID {
			if result.CustodyPending {
				lease.Status = "custody_pending"
			} else {
				lease.Status = "released"
			}
		}
		if wasPolicyFenced && !result.CustodyPending {
			pending := len(s.active[req.SessionID]) > 0
			if rec.Session.Lease != nil && rec.Session.Lease.Status == "custody_pending" {
				pending = true
			}
			for _, child := range rec.Session.Children {
				if child.Lease != nil && (child.Lease.Status == "custody_pending" || child.Lease.Status == "policy_fenced") {
					pending = true
				}
			}
			rec.Session.CustodyPending = pending
		}

		// A fenced writer cannot publish more events or claim success. Its
		// final stopped-workspace snapshot is still custody evidence worth
		// retaining, including partial edits made before cancellation.
		if req.ChildID == "" && lease != nil && lease.LeaseID == req.LeaseID {
			if result.WorkspacePath != "" {
				rec.Session.WorkspacePath = result.WorkspacePath
			}
			if result.Changes != nil {
				rec.Session.Changes = clone(result.Changes)
				rec.Session.Diff = s.clean(result.Diff)
				appendEvent(rec, "changes.updated", req.TurnID, "", map[string]any{"changes": rec.Session.Changes, "diff": rec.Session.Diff, "writerStopped": !result.CustodyPending})
			}
		}
		now := time.Now().UTC()
		rejectUnconfirmedCommands(rec, req)
		if result.CustodyPending {
			rec.Session.CustodyPending = true
			rec.Session.Status = "recovery_required"
			rec.Session.Fence++
			rec.Session.RecoveryReason = "Executor could not verify workspace quiescence; prior writer custody remains pending."
			for i := range rec.Session.Children {
				if rec.Session.Children[i].Status == "running" {
					rec.Session.Children[i].Status = "uncertain"
				}
			}

			if runErr != nil {
				rec.Session.RecoveryReason = s.clean(runErr.Error())
			}
			if req.ChildID == "" {
				rec.Session.ActiveTurnID = req.TurnID
				for i := range rec.Session.Turns {
					if rec.Session.Turns[i].ID == req.TurnID {
						rec.Session.Turns[i].Status = "interrupted"
						rec.Session.Turns[i].Error = rec.Session.RecoveryReason
						rec.Session.Turns[i].FinishedAt = nil
					}
				}
			} else {
				for i := range rec.Session.Children {
					if rec.Session.Children[i].ID == req.ChildID {
						rec.Session.Children[i].Status = "uncertain"
						rec.Session.Children[i].Result = rec.Session.RecoveryReason
					}
				}
			}
			appendEvent(rec, "recovery.required", req.TurnID, "", map[string]any{"reason": rec.Session.RecoveryReason, "custodyPending": true, "childId": req.ChildID})
			return nil
		}
		if req.ChildID != "" {
			for i := range rec.Session.Children {
				child := &rec.Session.Children[i]
				if child.ID != req.ChildID {
					continue
				}
				if rec.Session.Status == "stopping" {
					child.Status = "cancelled"
				} else if current {
					child.Status = "succeeded"
					child.Result = s.clean(result.Message)
					if runErr != nil {
						child.Status = "failed"
						child.Result = s.clean(runErr.Error())
					}
					if result.WaitingAuth {
						child.Status = "failed"
						child.Result = "Authentication required; child did not complete."
					}
				}
				appendEvent(rec, "child.completed", req.TurnID, "", map[string]any{"childId": child.ID, "status": child.Status, "result": child.Result, "readOnly": true})
			}
		} else if current {
			status := "completed"
			rec.Session.Status = "idle"
			rec.Session.ActiveTurnID = ""
			if result.WaitingAuth {
				status = "waiting_auth"
				rec.Session.Status = "waiting_auth"
				rec.Session.ActiveTurnID = req.TurnID
				appendEvent(rec, "auth.required", req.TurnID, "", map[string]any{"reason": s.clean(result.AuthReason)})
			} else if runErr != nil {
				status = "failed"
				rec.Session.Status = "error"
				if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
					status = "interrupted"
					rec.Session.Status = "recovery_required"
					rec.Session.ActiveTurnID = req.TurnID
					rec.Session.RecoveryReason = "Execution interrupted; writer stopped, explicit recovery required."
				}
				appendEvent(rec, "turn.failed", req.TurnID, "", map[string]any{"error": s.clean(runErr.Error()), "status": status})
			} else {
				if result.Message != "" {
					appendEvent(rec, "assistant.message", req.TurnID, "", map[string]any{"text": s.clean(result.Message)})
				}
				appendEvent(rec, "turn.completed", req.TurnID, "", map[string]any{"status": "completed"})
			}
			for i := range rec.Session.Turns {
				turn := &rec.Session.Turns[i]
				if turn.ID == req.TurnID {
					turn.Status = status
					turn.FinishedAt = &now
					if runErr != nil {
						turn.Error = s.clean(runErr.Error())
					}
				}
			}
		}
		if rec.Session.Status == "stopping" && !rec.Session.CustodyPending && !rec.Session.StopFencePending && len(s.active[req.SessionID]) == 0 {
			markStopped(rec)
		}
		return nil
	})
	if result.CustodyPending {
		for _, run := range s.active[req.SessionID] {
			run.cancel()
		}
	}
}

// CheckExecution is the final broker admission check. It never trusts a lease
// merely because its signature or original grant was once valid. A dispatched
// operation may still finish after this check and a subsequent fence; providers
// cannot participate in the coordinator's local transaction.
func (s *Service) CheckExecution(owner, sessionID, turnID, leaseID string, fence uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.checkExecutionLocked(owner, sessionID, turnID, leaseID, fence)
}

func (s *Service) checkExecutionLocked(owner, sessionID, turnID, leaseID string, fence uint64) error {
	if s.closed || s.fatal != nil {
		return problem(503, "unavailable", "execution coordinator unavailable")
	}
	rec, err := s.record(owner, sessionID)
	if err != nil {
		return err
	}
	if rec.Session.Fence != fence || rec.Session.Status == "stopping" || rec.Session.Status == "stopped" || rec.Session.Status == "recovery_required" {
		return problem(409, "stale_fence", "execution is fenced")
	}
	surface, exists := s.surface(rec.Session.SurfaceID)
	if !exists || !surface.Enabled || rec.Session.ConfigDigest != digest(surface) || !surfaceAllowsMode(surface, rec.Session.ExecutionMode) || !s.executionModeAvailable(rec.Session.ExecutionMode) {
		return problem(409, "surface_changed", "execution policy is no longer current")
	}
	for _, run := range s.active[sessionID] {
		if run.request.LeaseID != leaseID || run.request.TurnID != turnID {
			continue
		}
		lease := leaseFor(rec, run.request.ChildID)
		if lease != nil && lease.LeaseID == leaseID && lease.Status == "active" && lease.ExpiresAt.After(s.opts.Now()) {
			return nil
		}
	}
	return problem(409, "stale_lease", "execution attempt has no current writer lease")
}
