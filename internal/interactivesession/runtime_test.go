package interactivesession

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type sessionTestHost struct {
	mu                           sync.Mutex
	runs                         string
	started                      chan string
	finish                       chan struct{}
	response                     string
	joined                       bool
	reserved, restored, released int
}

func installTestRuntime(t *testing.T, s *Service) *sessionTestHost {
	t.Helper()
	h := &sessionTestHost{runs: t.TempDir(), started: make(chan string, 10), finish: make(chan struct{}), response: "Agent answer", joined: true}
	s.Runtime = &Runtime{Build: h.build, Observe: h.observe, Reserve: func(_ context.Context, _ journal.RunIdentity, _ time.Time) (func(), error) {
		h.mu.Lock()
		h.reserved++
		h.mu.Unlock()
		return h.release, nil
	}, Restore: func(_ journal.RunIdentity) (func(), error) {
		h.mu.Lock()
		h.restored++
		h.mu.Unlock()
		return h.release, nil
	}}
	return h
}
func (h *sessionTestHost) release() { h.mu.Lock(); h.released++; h.mu.Unlock() }
func (h *sessionTestHost) build(_ context.Context, t triggerqueue.SessionTurn, in sessioning.ExecutionInputs) (PreparedTurn, error) {
	runID := strings.TrimPrefix(t.Record.ID, "trigger-")
	raw, err := in.Validate(runID, t.Session.Gaggle)
	if err != nil {
		return PreparedTurn{}, err
	}
	id := journal.RunIdentity{RunID: runID, Gaggle: t.Session.Gaggle, Workflow: "interactive-session", WorkflowVersion: 1, WorkflowDigest: journal.Digest([]byte("machine")), GooberDigest: t.Session.GooberDigest, ConfigGeneration: t.Session.ConfigGeneration, Trigger: journal.Trigger{Kind: journal.TriggerSignal, Ref: "session:" + t.Session.ID + ":" + t.ID}, Session: &journal.SessionLineage{Gaggle: t.Session.Gaggle, SessionID: t.Session.ID, TurnID: t.ID, MessageID: t.Message.ID, AcceptanceID: t.Record.ID, EnvelopeDigest: journal.Digest(t.Record.Payload), InputDigest: journal.Digest(raw)}}
	return PreparedTurn{Identity: id, Release: func() {}, Run: func(ctx context.Context, _ *interactiveaccess.ExecutionLease, published func() error) error {
		run, err := journal.Create(h.runs, id, map[string][]byte{sessioning.ContextInputName: raw})
		if err != nil {
			return err
		}
		defer func() { _ = run.Close() }()
		if err = published(); err != nil {
			return err
		}
		h.started <- runID
		outcome := journal.PhaseCompleted
		select {
		case <-h.finish:
		case <-ctx.Done():
			outcome = journal.PhaseAborted
		}
		return run.Append(journal.Event{Type: journal.EventRunFinished, Status: string(outcome)})
	}}, nil
}
func (h *sessionTestHost) observe(ctx context.Context, t triggerqueue.SessionTurn, _ sessioning.ExecutionInputs) (Observation, error) {
	dir := filepath.Join(h.runs, strings.TrimPrefix(t.Record.ID, "trigger-"))
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return Observation{Absent: true}, nil
	} else if err != nil {
		return Observation{}, err
	}
	rd, err := journal.OpenReadOnly(dir)
	if err != nil {
		return Observation{}, err
	}
	id, err := rd.Identity()
	if err != nil {
		return Observation{}, err
	}
	phase, err := rd.PhaseBounded(ctx)
	if err != nil {
		return Observation{}, err
	}
	outcome := "success"
	if phase == journal.PhaseAborted {
		outcome = "cancelled"
	}
	h.mu.Lock()
	joined, response := h.joined, h.response
	h.mu.Unlock()
	return Observation{Found: true, Identity: id, Terminal: phase != journal.PhaseRunning, WritersJoined: joined, Outcome: outcome, Text: response}, nil
}
func queuedTurn(t *testing.T, s *Service) (sessioning.Acceptance, triggerqueue.SessionTurn) {
	t.Helper()
	created, err := s.Create(t.Context(), sessionPrincipal("alice"), "gaggle", sessioning.CreateRequest{RequestID: "create", Title: "Scope", Goober: "planner"})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := s.SubmitMessage(t.Context(), sessionPrincipal("alice"), "gaggle", created.Session.ID, sessioning.MessageRequest{RequestID: "first", Text: "First question"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := s.Queue.SessionTurn(t.Context(), accepted.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	return accepted, turn
}
func waitSessionStart(t *testing.T, h *sessionTestHost) {
	t.Helper()
	select {
	case <-h.started:
	case <-time.After(5 * time.Second):
		t.Fatal("turn never started")
	}
}

func TestSessionRuntimeDisconnectedBrowserOrderedTurnsAndVerifiedLinks(t *testing.T) {
	s, reg, _ := serviceFixture(t)
	h := installTestRuntime(t, s)
	reg.Register([]byte("secret-canary"))
	h.response = "Answer secret-canary"
	accepted, turn := queuedTurn(t, s)
	browser, cancel := context.WithCancel(t.Context())
	if err := s.Dispatch(browser, t.Context(), turn.Record); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitSessionStart(t, h)
	second, err := s.SubmitMessage(t.Context(), sessionPrincipal("bob"), "gaggle", accepted.Session.ID, sessioning.MessageRequest{RequestID: "second", Text: "Second question"})
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.Queue.SessionTurn(t.Context(), second.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Dispatch(t.Context(), t.Context(), next.Record); err != nil {
		t.Fatal(err)
	}
	if err = s.Dispatch(t.Context(), t.Context(), turn.Record); err != nil {
		t.Fatal(err)
	}
	page, err := s.Messages(t.Context(), sessionPrincipal("alice"), "gaggle", accepted.Session.ID, 0, 100)
	if err != nil || len(page.Items) != 2 || page.Items[0].RunID == "" || page.Items[1].RunID != "" {
		t.Fatal(page, err)
	}
	close(h.finish)
	s.Wait()
	if h.reserved != 1 || h.released != 1 {
		t.Fatal(h.reserved, h.released)
	}
	if err = s.Dispatch(t.Context(), t.Context(), next.Record); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	inputs, err := s.Queue.SessionInputs(t.Context(), next.Record.ID)
	if err != nil || len(inputs.Messages) != 3 || strings.Contains(inputs.Messages[1].Text, "secret-canary") || inputs.Messages[1].ActorKind != "agent" {
		t.Fatal(inputs, err)
	}
	if h.reserved != 2 || h.released != 2 {
		t.Fatal(h.reserved, h.released)
	}
}

func TestSessionRuntimeCloseCancelsAndJoinsBeforeReleasing(t *testing.T) {
	s, _, _ := serviceFixture(t)
	h := installTestRuntime(t, s)
	accepted, turn := queuedTurn(t, s)
	if err := s.Dispatch(t.Context(), t.Context(), turn.Record); err != nil {
		t.Fatal(err)
	}
	waitSessionStart(t, h)
	if _, err := s.Close(t.Context(), sessionPrincipal("bob"), "gaggle", accepted.Session.ID, sessioning.CloseRequest{RequestID: "close", Reason: "Stop this turn"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(t.Context(), turn.Record, false); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	saved, err := s.Queue.Session(t.Context(), "gaggle", accepted.Session.ID)
	if err != nil || saved.State != sessioning.Closed || saved.LastOutcome != "cancelled" || h.released != 1 {
		t.Fatal(saved, h.released, err)
	}
}

func TestSessionRuntimeUnknownWriterRetainsCustodyAcrossTerminalAndRestart(t *testing.T) {
	s, _, _ := serviceFixture(t)
	h := installTestRuntime(t, s)
	h.joined = false
	accepted, turn := queuedTurn(t, s)
	if err := s.Dispatch(t.Context(), t.Context(), turn.Record); err != nil {
		t.Fatal(err)
	}
	waitSessionStart(t, h)
	close(h.finish)
	s.Wait()
	current, err := s.Queue.SessionTurn(t.Context(), turn.Record.ID)
	if err != nil || current.State != "running" || h.released != 0 {
		t.Fatal(current.State, h.released, err)
	}
	// A new process has no live in-memory owner, but terminal events still do
	// not prove old writers stopped. It restores capacity and keeps the FIFO.
	restored := &Service{Queue: s.Queue, Permissions: s.Permissions, Scrubber: s.Scrubber, Pin: s.Pin, Now: s.Now, Runtime: s.Runtime}
	if err = restored.Reconcile(t.Context(), turn.Record, false); err != nil {
		t.Fatal(err)
	}
	if h.restored != 1 || h.released != 0 {
		t.Fatal(h.restored, h.released)
	}
	h.mu.Lock()
	h.joined = true
	h.mu.Unlock()
	if err = restored.Reconcile(t.Context(), turn.Record, false); err != nil {
		t.Fatal(err)
	}
	saved, err := s.Queue.Session(t.Context(), "gaggle", accepted.Session.ID)
	if err != nil || saved.State != sessioning.Idle || h.released != 1 {
		t.Fatal(saved, h.released, err)
	}
}

func TestSessionRuntimeCurrentPolicyRevocationRejectsUnstartedTurn(t *testing.T) {
	s, _, g := serviceFixture(t)
	h := installTestRuntime(t, s)
	_, turn := queuedTurn(t, s)
	g.Spec.InteractiveAccess.Actions = nil
	if err := s.Permissions.Apply([]apiv1.Gaggle{g}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(t.Context(), t.Context(), turn.Record); !errors.Is(err, interactiveaccess.ErrDenied) {
		t.Fatal(err)
	}
	saved, err := s.Queue.SessionTurn(t.Context(), turn.Record.ID)
	if err != nil || saved.State != "settled" || saved.Outcome != "rejected" || h.reserved != 0 {
		t.Fatal(saved, h.reserved, err)
	}
}

func TestSessionRuntimeAbsentRecoveryRequiresStartupProofAndExactProvenance(t *testing.T) {
	s, _, _ := serviceFixture(t)
	h := installTestRuntime(t, s)
	_, turn := queuedTurn(t, s)
	if _, err := s.Queue.SessionInputs(t.Context(), turn.Record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Queue.BeginSessionTurn(t.Context(), turn.Record.ID, s.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Reconcile(t.Context(), turn.Record, false); err != nil {
		t.Fatal(err)
	}
	saved, _ := s.Queue.SessionTurn(t.Context(), turn.Record.ID)
	if saved.State != "dispatching" {
		t.Fatal("absence alone replayed uncertain start")
	}
	if err := s.Reconcile(t.Context(), turn.Record, true); err != nil {
		t.Fatal(err)
	}
	saved, _ = s.Queue.SessionTurn(t.Context(), turn.Record.ID)
	if saved.State != "queued" {
		t.Fatal(saved.State)
	}
	inputs, err := s.Queue.SessionInputs(t.Context(), turn.Record.ID)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := h.build(t.Context(), turn, inputs)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Identity.Session.TurnID = "foreign"
	if err = verifyTurnIdentity(turn, inputs, prepared.Identity); err == nil {
		t.Fatal("foreign run accepted")
	}
}
