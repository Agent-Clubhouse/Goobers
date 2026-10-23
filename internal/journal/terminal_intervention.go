package journal

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TerminalIntervention is a trusted host's bounded HITL journal transition.
// The marker uses the local intervention vocabulary; it is also the durable
// idempotency receipt. Ordinary appends do not acquire this authority.
type TerminalIntervention struct {
	RequestID                  string  `json:"requestId"`
	Fingerprint                string  `json:"fingerprint"`
	ExpectedTerminalGeneration uint64  `json:"expectedTerminalGeneration"`
	ExpectedTerminalSeq        uint64  `json:"expectedTerminalSeq"`
	Events                     []Event `json:"events"`
}

// TerminalInterventionReceipt identifies the durable prefix and its run segment.
type TerminalInterventionReceipt struct {
	Seq       uint64 `json:"seq"`
	Segment   uint64 `json:"segment"`
	Resumed   bool   `json:"resumed"`
	Duplicate bool   `json:"duplicate"`
}

// TerminalInterventionBackend is intentionally separate from ordinary append.
// Implementations check the generation, payload binding and ordered prefix
// under the same owner fence as the append. A file prefix may be retried only
// by the identical intent; PG commits the entire transition in one transaction.
type TerminalInterventionBackend interface {
	CommitTerminalIntervention(context.Context, RunHandle, OwnerToken, TerminalIntervention) (TerminalInterventionReceipt, error)
}

// Terminal intervention errors distinguish malformed, conflicting, stale and
// incomplete intents without granting ordinary append authority.
var (
	ErrInterventionInvalid  = errors.New("journal: invalid terminal intervention")
	ErrInterventionConflict = errors.New("journal: terminal intervention key reused")
	ErrInterventionStale    = errors.New("journal: terminal intervention generation changed")
	ErrInterventionPending  = errors.New("journal: another terminal intervention has an incomplete prefix")
)

// ValidateTerminalIntervention admits only the local marker followed by its
// reopening event, or the single deny annotation which leaves a run terminal.
func ValidateTerminalIntervention(req TerminalIntervention) error {
	digest, ok := strings.CutPrefix(req.Fingerprint, "sha256:")
	decoded, err := hex.DecodeString(digest)
	if strings.TrimSpace(req.RequestID) == "" || !ok || err != nil || len(decoded) != 32 || req.ExpectedTerminalGeneration == 0 || req.ExpectedTerminalSeq == 0 || len(req.Events) < 1 || len(req.Events) > 2 {
		return ErrInterventionInvalid
	}
	marker := req.Events[0]
	if marker.Type != EventRunnerAnnotation || marker.Branch != 0 || marker.Runner["idempotencyKey"] != req.RequestID || marker.Runner["fingerprint"] != req.Fingerprint {
		return ErrInterventionInvalid
	}
	for _, event := range req.Events {
		key, _ := event.Runner["emitKey"].(string)
		if key == "" || event.Branch != 0 {
			return ErrInterventionInvalid
		}
	}
	if marker.Runner["kind"] == "escalation.resolution" {
		if len(req.Events) != 1 || marker.Runner["resolution"] != "deny" {
			return ErrInterventionInvalid
		}
		return nil
	}
	if marker.Runner["kind"] != "intervention.idempotency" || len(req.Events) != 2 {
		return ErrInterventionInvalid
	}
	event := req.Events[1]
	switch marker.Runner["action"] {
	case "rerun":
		if event.Type != EventStageRerunRequested || event.Stage == "" || event.Attempt < 1 {
			return ErrInterventionInvalid
		}
	case "approve", "override", "resume":
		if event.Type != EventRunResumed && event.Type != EventGateOverridden {
			return ErrInterventionInvalid
		}
	default:
		return ErrInterventionInvalid
	}
	if req.Events[0].Runner["emitKey"] == event.Runner["emitKey"] {
		return ErrInterventionInvalid
	}
	return nil
}

// TerminalInterventionCheck folds immutable history before a database fence,
// then only its concurrent tail under the append lock. No full-history scan is
// performed while holding the lease's bounded write transaction.
type TerminalInterventionCheck struct {
	request                 TerminalIntervention
	generation, terminalSeq uint64
	terminal                bool
	pending                 string
	matched                 int
	receipt                 TerminalInterventionReceipt
	err                     error
}

// NewTerminalInterventionCheck validates the request and folds committed history.
func NewTerminalInterventionCheck(req TerminalIntervention, events []Event) (*TerminalInterventionCheck, error) {
	if err := ValidateTerminalIntervention(req); err != nil {
		return nil, err
	}
	check := &TerminalInterventionCheck{request: req}
	check.Observe(events)
	return check, nil
}

// Observe folds a subsequent committed tail in sequence order under the fence.
func (c *TerminalInterventionCheck) Observe(events []Event) {
	for _, event := range events {
		c.observe(event)
	}
}
func (c *TerminalInterventionCheck) observe(event Event) {
	if c.err != nil {
		return
	}
	for _, expected := range c.request.Events {
		if event.Runner["emitKey"] == expected.Runner["emitKey"] && !sameInterventionEvent(event, expected) {
			c.err = ErrInterventionConflict
			return
		}
	}
	marker := event.Type == EventRunnerAnnotation && (event.Runner["kind"] == "intervention.idempotency" || event.Runner["kind"] == "escalation.resolution")
	if marker && event.Runner["idempotencyKey"] == c.request.RequestID {
		if c.matched != 0 || c.generation != c.request.ExpectedTerminalGeneration || c.terminalSeq != c.request.ExpectedTerminalSeq || !sameInterventionEvent(event, c.request.Events[0]) {
			c.err = ErrInterventionConflict
			return
		}
		c.matched = 1
		c.receipt = TerminalInterventionReceipt{Seq: event.Seq, Segment: c.generation, Resumed: len(c.request.Events) == 2}
		if c.receipt.Resumed {
			c.receipt.Segment++
		}
	} else if c.matched > 0 && c.matched < len(c.request.Events) {
		if !sameInterventionEvent(event, c.request.Events[c.matched]) {
			c.err = ErrInterventionConflict
			return
		}
		c.matched++
		c.receipt.Seq = event.Seq
	}
	if marker && event.Runner["kind"] == "intervention.idempotency" {
		c.pending, _ = event.Runner["idempotencyKey"].(string)
	}
	switch event.Type {
	case EventRunFinished:
		c.generation++
		c.terminalSeq = event.Seq
		c.terminal = event.Status == string(PhaseEscalated) || event.Status == string(PhaseFailed)
	case EventRunStarted, EventRunResumed, EventStageRerunRequested, EventGateOverridden:
		c.terminal = false
		c.pending = ""
	}
}

// Remaining returns the already-committed prefix length and its receipt.
// On a complete replay the receipt stays bound to the original segment even
// if later work has advanced the journal to another terminal.
func (c *TerminalInterventionCheck) Remaining() (int, TerminalInterventionReceipt, error) {
	if c.err != nil {
		return 0, TerminalInterventionReceipt{}, c.err
	}
	if c.matched == len(c.request.Events) {
		receipt := c.receipt
		receipt.Duplicate = true
		return c.matched, receipt, nil
	}
	if c.pending != "" && c.pending != c.request.RequestID {
		return 0, TerminalInterventionReceipt{}, ErrInterventionPending
	}
	if !c.terminal || c.generation != c.request.ExpectedTerminalGeneration || c.terminalSeq != c.request.ExpectedTerminalSeq {
		return 0, TerminalInterventionReceipt{}, ErrInterventionStale
	}
	receipt := TerminalInterventionReceipt{Segment: c.generation, Resumed: len(c.request.Events) == 2}
	if receipt.Resumed {
		receipt.Segment++
	}
	return c.matched, receipt, nil
}

func sameInterventionEvent(a, b Event) bool {
	// Sequence and time are backend-assigned. Everything else, including the
	// keyed marker's payload fingerprint, must survive identical retries.
	a.Seq, b.Seq = 0, 0
	a.Time, b.Time = time.Time{}, time.Time{}
	a.Schema, b.Schema = "", ""
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && bytes.Equal(left, right)
}

func interventionError(err error) error { return fmt.Errorf("commit terminal intervention: %w", err) }
