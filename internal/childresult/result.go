// Package childresult defines the bounded result wire contract shared by child
// custody and engine continuations. Validation checks shape and pinned identity;
// it does not authenticate references or grant workspace disposition authority.
package childresult

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/triggerqueue"
)

const (
	MaxSummaryBytes       = 8 << 10
	MaxReferences         = 16
	MaxReferenceBytes     = 1024
	TerminalReturnVersion = 1
	// MaxTerminalReturnBytes bounds the encoded envelope, including JSON escaping.
	// The summary limit alone is not a bound on a Temporal payload.
	MaxTerminalReturnBytes = 32 << 10
)

var (
	ErrInvalidTerminalInput  = errors.New("child result: invalid terminal input")
	ErrInvalidTerminalReturn = errors.New("child result: invalid terminal return")
)

// TerminalResultInput comes from the trusted execution observer after joining
// all writers. Summary is already redacted; references are bounded artifact
// identifiers, never instructions or authority. FinishedAt is durable journal
// or queue time, not a fresh wall-clock reading on retry.
type TerminalResultInput struct {
	State      triggerqueue.ChildState `json:"state"`
	FinishedAt time.Time               `json:"finishedAt"`
	Summary    string                  `json:"summary,omitempty"`
	References []string                `json:"references,omitempty"`
}

// Validate preserves the existing runner result-custody bounds.
func (in TerminalResultInput) Validate(acceptedAt time.Time) error {
	if !in.State.Terminal() || in.FinishedAt.IsZero() || in.FinishedAt.Before(acceptedAt) || len(in.Summary) > MaxSummaryBytes || !utf8.ValidString(in.Summary) || strings.ContainsRune(in.Summary, 0) || len(in.References) > MaxReferences {
		return ErrInvalidTerminalInput
	}
	for _, ref := range in.References {
		if !boundedText(ref, MaxReferenceBytes) {
			return ErrInvalidTerminalInput
		}
	}
	return nil
}

// TerminalReturn carries only a bounded summary and immutable references. The
// result receipt and workspace are verified by activities before continuation;
// neither full stage outputs nor snapshot/archive bytes belong in this DTO.
type TerminalReturn struct {
	Version      int                        `json:"version"`
	Identity     triggerqueue.ChildIdentity `json:"identity"`
	AcceptanceID string                     `json:"acceptanceId"`
	RunID        string                     `json:"runId"`
	SourceDigest string                     `json:"sourceDigest"`
	Input        TerminalResultInput        `json:"input"`
	ResultRef    string                     `json:"resultRef"`
	WorkspaceRef string                     `json:"workspaceRef,omitempty"`
}

// Validate binds a return to the accepted execution, including its current run
// (an execution epoch can change RunID without changing the logical child).
// Matching these pins is necessary but not proof of stored, stopped-writer custody.
func (r TerminalReturn) Validate(child triggerqueue.ChildRecord) error {
	if r.Version != TerminalReturnVersion || child.AcceptedAt.IsZero() ||
		r.Identity != child.Identity || r.AcceptanceID != child.AcceptanceID || r.RunID != child.RunID || r.SourceDigest != child.ProposalDigest ||
		!boundedText(r.Identity.Gaggle, 128) || !boundedText(r.Identity.ParentRunID, 256) || !boundedText(r.Identity.StageOccurrence, 256) || !boundedText(r.Identity.InvocationKey, 256) ||
		!boundedText(r.AcceptanceID, 256) || !boundedText(r.RunID, 256) || !blobstore.ValidDigest(r.SourceDigest) || !blobstore.ValidDigest(r.ResultRef) ||
		(r.WorkspaceRef != "" && !boundedText(r.WorkspaceRef, MaxReferenceBytes)) || r.Input.Validate(child.AcceptedAt) != nil {
		return ErrInvalidTerminalReturn
	}
	encoded, err := json.Marshal(r)
	if err != nil || len(encoded) > MaxTerminalReturnBytes {
		return ErrInvalidTerminalReturn
	}
	return nil
}

func boundedText(value string, max int) bool {
	return value != "" && len(value) <= max && utf8.ValidString(value) && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) == -1
}
