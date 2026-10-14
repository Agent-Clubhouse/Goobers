// Package fanout implements the durable half of DSL 3.1 data-driven fan-out
// (docs/design/plan-driven-dynamic-fan-out.md): freezing a map's roster source
// into the normalized roster map.started journals (§4.2, §6.2), and verifying
// a journaled map visit against that roster before anything resumes it (§8).
//
// Everything here is a pure function of bytes and journal events. Resolving
// the source artifact from the producer's attempt, appending events, and
// executing items belong to the runner.
package fanout

import (
	"errors"
	"fmt"
)

// Stable error codes (design §4.2 and §8).
const (
	// CodeSourceInvalid is the fan-out state's stage failure when its source
	// document cannot be frozen. It takes the fan-out's onFailure route, or
	// @abort, and is never a silent empty fan-out.
	CodeSourceInvalid = "fanout_source_invalid"
	// CodeWorkflowIntegrity refuses to resume a fan-out whose journal records
	// are missing, extra, duplicated or in conflict with the frozen roster. It
	// is never repaired by rerunning the source task.
	CodeWorkflowIntegrity = "workflow_integrity"
)

var (
	// ErrSourceInvalid classifies every *SourceError with errors.Is.
	ErrSourceInvalid = errors.New(CodeSourceInvalid)
	// ErrWorkflowIntegrity classifies every *IntegrityError with errors.Is.
	ErrWorkflowIntegrity = errors.New(CodeWorkflowIntegrity)
)

// Source failure reasons. Each is a stable ErrorClass of a *SourceError.
const (
	ReasonProducer          = "producer"
	ReasonMediaType         = "media_type"
	ReasonTooLarge          = "too_large"
	ReasonEnvelope          = "envelope"
	ReasonIdentity          = "identity"
	ReasonDuplicateIdentity = "duplicate_identity"
	ReasonMalformedDigest   = "malformed_digest"
	ReasonDigestMismatch    = "digest_mismatch"
	ReasonItem              = "item"
)

// SourceError is a typed fanout_source_invalid failure.
type SourceError struct {
	// Reason is one of the Reason* constants.
	Reason string
	// Identity names the offending roster item, when one is known.
	Identity string
	// Detail is human-facing context.
	Detail string
}

func (e *SourceError) Error() string {
	if e.Identity != "" {
		return fmt.Sprintf("%s: %s: item %q: %s", CodeSourceInvalid, e.Reason, e.Identity, e.Detail)
	}
	return fmt.Sprintf("%s: %s: %s", CodeSourceInvalid, e.Reason, e.Detail)
}

// Unwrap exposes ErrSourceInvalid to errors.Is.
func (e *SourceError) Unwrap() error { return ErrSourceInvalid }

// ErrorCode is the stable journal error code.
func (e *SourceError) ErrorCode() string { return CodeSourceInvalid }

// ErrorClass is the stable failure reason.
func (e *SourceError) ErrorClass() string { return e.Reason }

// Integrity failure kinds (design §8 step 5). Each is a stable ErrorClass of
// an *IntegrityError.
const (
	// KindMissing: a record that must precede another is absent, or a
	// completeness record omits a roster item.
	KindMissing = "missing"
	// KindExtra: a record names a branch outside the frozen roster.
	KindExtra = "extra"
	// KindDuplicate: a record that must occur once per item or visit repeats.
	KindDuplicate = "duplicate"
	// KindConflicting: a record disagrees with the frozen roster or the
	// pinned definition (identity, digest, mapped task, or bounds).
	KindConflicting = "conflicting"
)

// IntegrityError is a typed workflow_integrity resume refusal.
type IntegrityError struct {
	// Kind is one of the Kind* constants.
	Kind string
	// FanOut names the map (or parallel) visit being reconstructed.
	FanOut string
	// Branch is the numeric branch the offending record names; 0 when the
	// record is the visit's own.
	Branch int
	// Seq is the offending event's sequence, when the record is an event.
	Seq uint64
	// Detail is human-facing context.
	Detail string
}

func (e *IntegrityError) Error() string {
	return fmt.Sprintf("%s: %s record in fan-out %q (branch %d, seq %d): %s; resume is refused and the source task is not rerun",
		CodeWorkflowIntegrity, e.Kind, e.FanOut, e.Branch, e.Seq, e.Detail)
}

// Unwrap exposes ErrWorkflowIntegrity to errors.Is.
func (e *IntegrityError) Unwrap() error { return ErrWorkflowIntegrity }

// ErrorCode is the stable journal error code.
func (e *IntegrityError) ErrorCode() string { return CodeWorkflowIntegrity }

// ErrorClass is the stable failure kind.
func (e *IntegrityError) ErrorClass() string { return e.Kind }
