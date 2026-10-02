package livejournal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// ControllerJournalTokenPrefix identifies exact-batch controller authority.
const ControllerJournalTokenPrefix = "goobers-controller-journal."

// ControllerJournalTTL bounds a controller's emission capability.
const ControllerJournalTTL = 2 * time.Minute

// ControllerStartProofField is reserved. The writer removes caller values and
// only stamps a MAC proof for a controller-authorized start on an owned journal.
const ControllerStartProofField = "controllerStartProof"

// ControllerStartAuthority seals and verifies durable origin, separately from
// short-lived HTTP authority. Proofs are not bearer credentials.
type ControllerStartAuthority interface {
	SealControllerStart(string, string, journal.Event) string
	VerifyControllerStart(string, string, journal.Event, string) bool
}

// WithControllerStartAuthority enables verifiable lifecycle-origin proofs.
func WithControllerStartAuthority(authority ControllerStartAuthority) Option {
	return func(w *Writer) { w.startAuthority = authority }
}

type controllerEmissionKey struct{}

// EmitController is the trusted in-process controller seam. HTTP handlers may
// enter it only after checking a distinct, exact-batch controller capability.
func (w *Writer) EmitController(ctx context.Context, req EmitRequest) (EmitResponse, error) {
	return w.Emit(context.WithValue(ctx, controllerEmissionKey{}, true), req)
}

func (w *Writer) stampControllerStart(ctx context.Context, runID string, run *liveRun, key string, ev *journal.Event) {
	trusted, _ := ctx.Value(controllerEmissionKey{}).(bool)
	if !trusted || run.adopted || w.startAuthority == nil || (ev.Type != journal.EventStageStarted && ev.Type != journal.EventReviewerStarted) {
		return
	}
	accepted := *ev
	accepted.Seq = run.jr.Seq() + 1 // This writer owns the handle under run.mu.
	ev.Runner[ControllerStartProofField] = w.startAuthority.SealControllerStart(runID, key, accepted)
}

// ControllerJournalDigest addresses the decoded request canonically, so the
// authenticated capability cannot authorize altered run IDs or batch contents.
func ControllerJournalDigest(req EmitRequest) (string, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// ControllerJournalMinter signs one exact controller emission request.
type ControllerJournalMinter interface {
	MintControllerJournal(string, string, time.Duration) (string, error)
}

// ControllerJournalVerifier validates a batch-bound credential and expiry.
type ControllerJournalVerifier interface {
	VerifyControllerJournal(string) (string, string, error)
}
