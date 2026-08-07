package journal

import (
	"fmt"
	"sort"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

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
	Schema              string
	Type                EventType
	Branch              int
	Stage               string
	Attempt             int
	AttemptClass        AttemptClass
	Actor               string
	Action              string
	Decision            string
	Rationale           string
	InstructionAddendum string
	Gate                string
	Verdict             string
	Target              string
	Complete            bool
	Escalated           bool
	Status              string
	WorkflowVersion     int
	WorkflowDigest      string
	RefDigest           string
	RefIntegrity        apiv1.Integrity
	Artifacts           string
	Name                string
	Integrity           apiv1.Integrity
	MinimumIntegrity    apiv1.Integrity

	// Outputs is a stable encoding of a stage.finished event's scalar-only
	// Outputs (event.go's doc comment: fully normative). Encoded rather than
	// kept as map[string]any so NormativeEvent stays flat and ==-comparable
	// (a struct containing a map is not comparable at all).
	Outputs string

	// Parallel/branch identity (§6.2). Completeness is a FLATTENED encoding of
	// the branch completeness record rather than a slice, because
	// NormativeEvent must stay directly ==-comparable; see
	// encodeCompleteness for the (stable, declaration-ordered) format.
	Parallel     string
	BranchName   string
	BranchStatus BranchStatus
	Completeness string

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
// attempts, stage.heartbeat, gate.started, gate.paused, span.recorded, repaired)
// and, on the events that remain, the fields event.go's doc comments mark
// non-normative. It is the single sanctioned comparison surface — the
// walking-skeleton seed (test/e2e/walking_skeleton_test.go) and the eventual V2
// conformance harness (#40) both go through this, not a test-local formatter,
// so a field added to the normative set here is automatically covered
// everywhere that compares.
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
		Attempt: e.Attempt, AttemptClass: e.AttemptClass,
		Actor: e.Actor, Action: e.Action, Decision: e.Decision, Rationale: e.Rationale,
		InstructionAddendum: e.InstructionAddendum,
		Gate:                e.Gate, Verdict: e.Verdict, Target: e.Target, Complete: e.Complete, Escalated: e.Escalated,
		Status: e.Status, WorkflowVersion: e.WorkflowVersion,
		WorkflowDigest: e.WorkflowDigest, Name: e.Name,
		Integrity: e.Integrity, MinimumIntegrity: e.MinimumIntegrity,
		Artifacts: encodeArtifactRefs(e.Artifacts),
		Parallel:  e.Parallel, BranchName: e.BranchName,
		BranchStatus: e.BranchStatus,
		Completeness: encodeCompleteness(e.Completeness),
		Outputs:      encodeOutputs(e.Outputs),
	}
	if e.Ref != nil {
		ne.RefIntegrity = e.Ref.Integrity
		if !isContextManifestArtifact(e) {
			ne.RefDigest = e.Ref.Digest
		}
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

// encodeArtifactRefs flattens the normative fields of stage.finished artifact
// pointers in their envelope order. Storage metadata remains excluded.
// ORDER-preserving, not sorted — production order is itself normative (it is
// what a branch-qualified inputsFrom union reads).
func encodeArtifactRefs(refs []Ref) string {
	if len(refs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, fmt.Sprintf("%s@%s", ref.Digest, ref.Integrity))
	}
	return strings.Join(parts, ",")
}

// encodeOutputs flattens a stage's scalar-only Outputs into a stable string,
// KEY-SORTED because map iteration order is not itself meaningful (unlike
// encodeCompleteness's declaration order, which assigns branch ids).
func encodeOutputs(outputs map[string]any) string {
	if len(outputs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(outputs))
	for k := range outputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, outputs[k]))
	}
	return strings.Join(parts, ",")
}

// encodeCompleteness flattens a branch completeness record into a stable
// string. Entries are emitted in the record's own (declaration) order — NOT
// sorted — because declaration order is itself normative: it is what assigns
// branch ids.
func encodeCompleteness(record []BranchOutcome) string {
	if len(record) == 0 {
		return ""
	}
	parts := make([]string, 0, len(record))
	for _, outcome := range record {
		parts = append(parts, fmt.Sprintf("%d:%s:%s:%d", outcome.Branch, outcome.Name, outcome.Status, outcome.Artifacts))
	}
	return strings.Join(parts, ",")
}

// ConformanceBranches groups a normative view by branch id, preserving
// within-branch order.
//
// This is the comparison surface for a run that forks. Absolute `seq` is NOT
// comparable across distinct non-zero branches (ARCHITECTURE §3.3): branch
// interleaving is a scheduling artefact, so two conformant runners may
// interleave differently and still be equivalent. Within a branch — and for
// the root branch, and for every run that never forks — ordering is total and
// fully normative, which is exactly what this grouping preserves.
func ConformanceBranches(events []Event) map[int][]NormativeEvent {
	out := map[int][]NormativeEvent{}
	for _, e := range events {
		if !e.IsConformanceNormative() {
			continue
		}
		out[e.Branch] = append(out[e.Branch], projectNormative(e))
	}
	return out
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
		"schema=%s|type=%s|branch=%d|stage=%s|attempt=%d|class=%s|actor=%s|action=%s|decision=%s|rationale=%s|addendum=%s|gate=%s|verdict=%s|target=%s|complete=%t|escalated=%t|status=%s|workflowVersion=%d|workflowDigest=%s|name=%s|ref=%s|refIntegrity=%s|artifacts=%s|integrity=%s|minIntegrity=%s|ext=%s|err=%s|redact=%s|parallel=%s|branchName=%s|branchStatus=%s|completeness=%s|outputs=%s",
		ne.Schema, ne.Type, ne.Branch, ne.Stage, ne.Attempt, ne.AttemptClass,
		ne.Actor, ne.Action, ne.Decision, ne.Rationale, ne.InstructionAddendum,
		ne.Gate, ne.Verdict, ne.Target, ne.Complete, ne.Escalated, ne.Status,
		ne.WorkflowVersion, ne.WorkflowDigest, ne.Name, ne.RefDigest, ne.RefIntegrity, ne.Artifacts,
		ne.Integrity, ne.MinimumIntegrity, ext, ne.ErrorCode, redaction,
		ne.Parallel, ne.BranchName, ne.BranchStatus, ne.Completeness, ne.Outputs,
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
