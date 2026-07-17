package journal

import "fmt"

// NormativeEvent is the cross-runner comparable projection of an Event: the
// full conformance-normative field set (§3.3), with excluded fields (Time,
// Ref.Path/Size/MediaType, context-manifest Ref.Digest, Error.Message,
// ExternalRef.URL, and the entire Runner map) dropped. Two runners implementing
// the same workflow definition against the same inputs must produce identical
// NormativeEvent sequences (ConformanceView's output) for identical outcomes —
// this is the comparison surface a V2 Temporal harness (#40) diffs against the
// local runner.
//
// Deliberately flat (every field a string/int/named-string type, no
// pointers) so NormativeEvent is directly comparable with == — a pointer
// field would compare identity, not content, which is exactly the footgun
// Event itself avoids by being flat (event.go's own doc comment). ExternalRef
// and Redaction, both optional on Event, are projected into empty-when-absent
// scalar fields rather than *ExternalRef/*RedactionInfo for the same reason.
type NormativeEvent struct {
	Schema       string
	Type         EventType
	Branch       int
	Stage        string
	Attempt      int
	AttemptClass AttemptClass
	Gate         string
	Verdict      string
	Target       string
	Status       string
	RefDigest    string
	Name         string

	// ExternalRef* project ExternalRef's normative identity (Provider, Kind,
	// ID); URL is dropped. All empty when the source event has no ExternalRef.
	ExternalRefProvider string
	ExternalRefKind     string
	ExternalRefID       string

	ErrorCode string

	// Redaction* project RedactionInfo (entirely normative per event.go).
	// All empty when the source event has no Redaction.
	RedactionTarget    string
	RedactionOldDigest string
	RedactionNewDigest string
	RedactionReason    string
}

// ConformanceView projects events down to the conformance-normative field set
// (§3.3): it drops events IsConformanceNormative excludes (infra-retry
// attempts, gate.started, span.recorded, repaired) and, on the events that
// remain, the fields event.go's doc comments mark non-normative. It is the single
// sanctioned comparison surface — the walking-skeleton seed
// (test/e2e/walking_skeleton_test.go) and the eventual V2 conformance harness
// (#40) both go through this, not a test-local formatter, so a field added to
// the normative set here is automatically covered everywhere that compares.
func ConformanceView(events []Event) []NormativeEvent {
	out := make([]NormativeEvent, 0, len(events))
	for _, e := range events {
		if !e.IsConformanceNormative() {
			continue
		}
		out = append(out, projectNormative(e))
	}
	return out
}

func projectNormative(e Event) NormativeEvent {
	ne := NormativeEvent{
		Schema: e.Schema, Type: e.Type, Branch: e.Branch, Stage: e.Stage,
		Attempt: e.Attempt, AttemptClass: e.AttemptClass, Gate: e.Gate,
		Verdict: e.Verdict, Target: e.Target, Status: e.Status, Name: e.Name,
	}
	if e.Ref != nil && !isContextManifestArtifact(e) {
		ne.RefDigest = e.Ref.Digest
	}
	if e.ExternalRef != nil {
		ne.ExternalRefProvider = e.ExternalRef.Provider
		ne.ExternalRefKind = e.ExternalRef.Kind
		ne.ExternalRefID = e.ExternalRef.ID
		// URL is deliberately not projected — not conformance-normative.
	}
	if e.Error != nil {
		ne.ErrorCode = e.Error.Code
	}
	if e.Redaction != nil {
		ne.RedactionTarget = e.Redaction.Target
		ne.RedactionOldDigest = e.Redaction.OldDigest
		ne.RedactionNewDigest = e.Redaction.NewDigest
		ne.RedactionReason = e.Redaction.Reason
	}
	return ne
}

func isContextManifestArtifact(e Event) bool {
	return e.Type == EventArtifactRecorded &&
		e.Stage != "" &&
		e.Attempt > 0 &&
		e.Name == ContextManifestArtifactName(e.Stage, e.Attempt)
}

// String renders ne as a stable, single-line, human-readable form for test
// diffs and debug output. Every field participates, so two NormativeEvents
// with a String() collision are conformance-equal by construction.
func (ne NormativeEvent) String() string {
	ext := fmt.Sprintf("%s:%s:%s", ne.ExternalRefProvider, ne.ExternalRefKind, ne.ExternalRefID)
	redaction := fmt.Sprintf("%s:%s->%s:%s", ne.RedactionTarget, ne.RedactionOldDigest, ne.RedactionNewDigest, ne.RedactionReason)
	return fmt.Sprintf(
		"schema=%s|type=%s|branch=%d|stage=%s|attempt=%d|class=%s|gate=%s|verdict=%s|target=%s|status=%s|name=%s|ref=%s|ext=%s|err=%s|redact=%s",
		ne.Schema, ne.Type, ne.Branch, ne.Stage, ne.Attempt, ne.AttemptClass,
		ne.Gate, ne.Verdict, ne.Target, ne.Status, ne.Name, ne.RefDigest, ext, ne.ErrorCode, redaction,
	)
}

// MonotonicSeq reports whether events' Seq values are exactly 1..N with no
// gaps, repeats, or reordering — the contract appendEvent's shared
// increment-then-assign counter guarantees for every event a run journals,
// including ones ConformanceView excludes (infra attempts, spans, repairs
// all still consume a seq). A real conformance harness comparing runners
// should assert this on every journal it reads, not just diff the normative
// view — seq monotonicity is a structural invariant of the journal itself,
// independent of which events are conformance-normative.
func MonotonicSeq(events []Event) error {
	for i, e := range events {
		want := uint64(i + 1)
		if e.Seq != want {
			return fmt.Errorf("journal: event %d (%s) has seq %d, want %d", i, e.Type, e.Seq, want)
		}
	}
	return nil
}
