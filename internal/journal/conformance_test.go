package journal

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// baseNormativeEvent is a fully-populated stage.finished event covering every
// field ConformanceView's doc comment claims is normative, plus every
// non-normative field it claims is excluded. Each subtest below mutates
// exactly one field off of this baseline.
func baseNormativeEvent() Event {
	return Event{
		Schema:       "v1",
		Seq:          3,
		Type:         EventStageFinished,
		Branch:       0,
		Time:         time.Date(2026, 7, 13, 5, 0, 0, 0, time.UTC),
		Stage:        "implement",
		Attempt:      1,
		AttemptClass: AttemptPolicy,
		Gate:         "review",
		Verdict:      "pass",
		Target:       "local-ci",
		Status:       "success",
		Ref:          &Ref{Path: "artifacts/sha256/aa/bb", Digest: "sha256:aaaa", Size: 42, MediaType: "text/plain"},
		Name:         "plan.txt",
		ExternalRef:  &ExternalRef{Provider: "github", Kind: "issue", ID: "101", URL: "https://x/101"},
		Error:        &ErrorDetail{Code: "executor_error", Message: "human-facing detail"},
		Redaction:    &RedactionInfo{Target: "artifacts/x", OldDigest: "sha256:old", NewDigest: "sha256:new", Reason: "leaked token"},
		Runner:       map[string]any{"repassAttempt": 2},
	}
}

// TestConformanceViewCapturesFullNormativeFieldSet is the "real
// failing-then-passing test" issue #141 asks for: before ConformanceView
// existed, the walking-skeleton seed's test-local formatter
// (canonicalizeNormative/fmtNormative) only compared Type/Stage/Gate/
// Verdict/Target/Status/Name/Ref.Digest/Error.Code — a diff purely in
// Attempt, AttemptClass, ExternalRef, Branch, Schema, or Redaction would have
// gone undetected (two non-conformant runners would have compared equal).
// This table mutates the baseline event one normative field at a time and
// asserts ConformanceView distinguishes every one of them — each subtest
// below would fail under the old 9-field comparison and passes under this
// one.
func TestConformanceViewCapturesFullNormativeFieldSet(t *testing.T) {
	base := baseNormativeEvent()
	cases := []struct {
		name   string
		mutate func(e Event) Event
	}{
		{"Schema", func(e Event) Event { e.Schema = "v2"; return e }},
		{"Type", func(e Event) Event { e.Type = EventStageStarted; return e }},
		{"Branch", func(e Event) Event { e.Branch = 1; return e }},
		{"Stage", func(e Event) Event { e.Stage = "local-ci"; return e }},
		{"Attempt", func(e Event) Event { e.Attempt = 2; return e }},
		{"AttemptClass", func(e Event) Event { e.AttemptClass = ""; return e }},
		{"Gate", func(e Event) Event { e.Gate = "other-gate"; return e }},
		{"Verdict", func(e Event) Event { e.Verdict = "needs-changes"; return e }},
		{"Target", func(e Event) Event { e.Target = "implement"; return e }},
		{"Status", func(e Event) Event { e.Status = "failure"; return e }},
		{"Name", func(e Event) Event { e.Name = "other.txt"; return e }},
		{"RefDigest", func(e Event) Event { r := *e.Ref; r.Digest = "sha256:cccc"; e.Ref = &r; return e }},
		{"ExternalRef.Provider", func(e Event) Event { r := *e.ExternalRef; r.Provider = "ado"; e.ExternalRef = &r; return e }},
		{"ExternalRef.Kind", func(e Event) Event { r := *e.ExternalRef; r.Kind = "pr"; e.ExternalRef = &r; return e }},
		{"ExternalRef.ID", func(e Event) Event { r := *e.ExternalRef; r.ID = "202"; e.ExternalRef = &r; return e }},
		{"ExternalRef presence", func(e Event) Event { e.ExternalRef = nil; return e }},
		{"ErrorCode", func(e Event) Event { r := *e.Error; r.Code = "other_code"; e.Error = &r; return e }},
		{"Redaction.Target", func(e Event) Event { r := *e.Redaction; r.Target = "artifacts/y"; e.Redaction = &r; return e }},
		{"Redaction.OldDigest", func(e Event) Event { r := *e.Redaction; r.OldDigest = "sha256:other"; e.Redaction = &r; return e }},
		{"Redaction.NewDigest", func(e Event) Event { r := *e.Redaction; r.NewDigest = "sha256:other"; e.Redaction = &r; return e }},
		{"Redaction presence", func(e Event) Event { e.Redaction = nil; return e }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutated := c.mutate(baseNormativeEvent())
			viewA := ConformanceView([]Event{base})
			viewB := ConformanceView([]Event{mutated})
			if viewA[0] == viewB[0] {
				t.Fatalf("ConformanceView did not distinguish a %s-only diff: both projected to %+v", c.name, viewA[0])
			}
			if viewA[0].String() == viewB[0].String() {
				t.Fatalf("NormativeEvent.String() did not distinguish a %s-only diff: both rendered %q", c.name, viewA[0].String())
			}
		})
	}
}

// TestConformanceViewExcludesNonNormativeFields is the mirror of the test
// above: mutating a field event.go's doc comments mark EXCLUDED must NOT
// change ConformanceView's output, or two conformant runners (differing only
// in wall-clock time, blob paths/sizes, human error text, or runner.*
// scheduling annotations) would spuriously fail comparison.
func TestConformanceViewExcludesNonNormativeFields(t *testing.T) {
	base := baseNormativeEvent()
	cases := []struct {
		name   string
		mutate func(e Event) Event
	}{
		{"Time", func(e Event) Event { e.Time = e.Time.Add(time.Hour); return e }},
		{"Seq", func(e Event) Event { e.Seq = 99; return e }},
		{"Ref.Path", func(e Event) Event { r := *e.Ref; r.Path = "artifacts/sha256/cc/dd"; e.Ref = &r; return e }},
		{"Ref.Size", func(e Event) Event { r := *e.Ref; r.Size = 999; e.Ref = &r; return e }},
		{"Ref.MediaType", func(e Event) Event { r := *e.Ref; r.MediaType = "application/json"; e.Ref = &r; return e }},
		{"ExternalRef.URL", func(e Event) Event { r := *e.ExternalRef; r.URL = "https://other/101"; e.ExternalRef = &r; return e }},
		{"Error.Message", func(e Event) Event {
			r := *e.Error
			r.Message = "a totally different human explanation"
			e.Error = &r
			return e
		}},
		{"Runner map", func(e Event) Event { e.Runner = map[string]any{"different": "value"}; return e }},
		{"Runner presence", func(e Event) Event { e.Runner = nil; return e }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mutated := c.mutate(baseNormativeEvent())
			viewA := ConformanceView([]Event{base})
			viewB := ConformanceView([]Event{mutated})
			if viewA[0] != viewB[0] {
				t.Fatalf("ConformanceView spuriously distinguished a %s-only diff (excluded field leaked in): %+v vs %+v", c.name, viewA[0], viewB[0])
			}
		})
	}
}

func TestConformanceViewExcludesContextManifestDigest(t *testing.T) {
	base := Event{
		Schema:  EventSchema,
		Type:    EventArtifactRecorded,
		Stage:   "implement",
		Attempt: 1,
		Name:    ContextManifestArtifactName("implement", 1),
		Ref:     &Ref{Digest: "sha256:aaaa"},
	}
	other := base
	other.Ref = &Ref{Digest: "sha256:bbbb"}

	got := ConformanceView([]Event{base})
	want := ConformanceView([]Event{other})
	if len(got) != 1 || len(want) != 1 {
		t.Fatalf("context manifest projections = %d and %d, want one each", len(got), len(want))
	}
	if got[0] != want[0] {
		t.Fatalf("context manifest content digest leaked into conformance: %+v vs %+v", got[0], want[0])
	}
	if got[0].RefDigest != "" {
		t.Fatalf("context manifest RefDigest = %q, want excluded", got[0].RefDigest)
	}

	ordinary := base
	ordinary.Name = "diff"
	ordinaryOther := other
	ordinaryOther.Name = "diff"
	if ConformanceView([]Event{ordinary})[0] == ConformanceView([]Event{ordinaryOther})[0] {
		t.Fatal("ordinary artifact content digest must remain normative")
	}
}

// TestConformanceViewSkipsExcludedEvents confirms ConformanceView filters
// through IsConformanceNormative — infra-tagged attempts, span.recorded, and
// repaired events never appear in the projection.
func TestConformanceViewSkipsExcludedEvents(t *testing.T) {
	events := []Event{
		{Type: EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: EventStageFinished, Stage: "implement", Attempt: 1, AttemptClass: AttemptInfra, Status: "failure"},
		{Type: EventStageStarted, Stage: "implement", Attempt: 2, AttemptClass: AttemptPolicy},
		{Type: EventStageFinished, Stage: "implement", Attempt: 2, AttemptClass: AttemptPolicy, Status: "success"},
		{Type: EventSpanRecorded, Ref: &Ref{Digest: "sha256:span"}},
		{Type: EventRepaired},
	}
	view := ConformanceView(events)
	if len(view) != 3 {
		t.Fatalf("ConformanceView returned %d events, want 3 (infra attempt, span, repaired excluded): %+v", len(view), view)
	}
	for _, ne := range view {
		if ne.AttemptClass == AttemptInfra {
			t.Errorf("infra-tagged event leaked through: %+v", ne)
		}
		if ne.Type == EventSpanRecorded || ne.Type == EventRepaired {
			t.Errorf("excluded event type leaked through: %+v", ne)
		}
	}
}

// TestConformanceViewReplayedProviderFixtureEmitsRefTouched is the
// "provider-backed (replayed/fake) fixture emitting ref.touched" issue #141
// asks for. No production code path constructs a real GitHubProvider yet
// (tracked separately under epic #130's provider-wiring cluster) — this
// replays a canned sequence of journal events, exactly the shape a
// provider-backed stage would append, directly through journal.Create/Append
// (the same primitive test/e2e's simulateSkeletonCrashMidImplement uses for
// its own hand-built fixture), so ConformanceView's ExternalRef handling is
// exercised end-to-end through a real on-disk journal, not just in-memory
// Event literals.
func TestConformanceViewReplayedProviderFixtureEmitsRefTouched(t *testing.T) {
	replay := func(t *testing.T) []Event {
		t.Helper()
		root := t.TempDir()
		id := testIdentity()
		// t.Name() includes a "/" for a subtest (e.g. replay2ndRun's
		// "second-replay") — a run id must be a single path segment
		// (#244), so collapse it to "-" rather than passing t.Name()
		// through raw.
		id.RunID = "replay-" + strings.ReplaceAll(t.Name(), "/", "-")
		run, err := Create(root, id, nil, WithClock(fixedClock()))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		seq := []Event{
			{Type: EventRefTouched, ExternalRef: &ExternalRef{Provider: "github", Kind: "issue", ID: "101", URL: "https://x/101"}},
			{Type: EventStageStarted, Stage: "implement", Attempt: 1},
			{Type: EventStageFinished, Stage: "implement", Attempt: 1, Status: "success"},
			{Type: EventRefTouched, ExternalRef: &ExternalRef{Provider: "github", Kind: "pr", ID: "202", URL: "https://x/pull/202"}},
			{Type: EventRunFinished, Status: string(PhaseCompleted)},
		}
		for _, ev := range seq {
			if err := run.Append(ev); err != nil {
				t.Fatalf("Append %s: %v", ev.Type, err)
			}
		}
		if err := run.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		rd, err := OpenRead(filepath.Join(root, id.RunID))
		if err != nil {
			t.Fatalf("OpenRead: %v", err)
		}
		events, err := rd.Events()
		if err != nil {
			t.Fatalf("Events: %v", err)
		}
		return events
	}

	events := replay(t)
	if err := MonotonicSeq(events); err != nil {
		t.Fatalf("MonotonicSeq: %v", err)
	}

	view := ConformanceView(events)
	var refs []NormativeEvent
	for _, ne := range view {
		if ne.Type == EventRefTouched {
			refs = append(refs, ne)
		}
	}
	if len(refs) != 2 {
		t.Fatalf("ref.touched count in ConformanceView = %d, want 2: %+v", len(refs), view)
	}
	if refs[0].ExternalRefProvider != "github" || refs[0].ExternalRefKind != "issue" || refs[0].ExternalRefID != "101" {
		t.Errorf("first ref.touched projected = %+v, want github/issue/101", refs[0])
	}
	// NormativeEvent structurally has no URL field at all — a source
	// ExternalRef.URL cannot leak into the projection, by construction.
	if refs[1].ExternalRefProvider != "github" || refs[1].ExternalRefKind != "pr" || refs[1].ExternalRefID != "202" {
		t.Errorf("second ref.touched projected = %+v, want github/pr/202", refs[1])
	}

	// Two independent replays of the identical provider interaction sequence
	// (same fixed clock, different RunID so they don't collide on disk) must
	// project to conformance-equal views — proving ConformanceView, not just
	// this test's own event literals, is what makes the replay comparable.
	other := replay2ndRun(t, replay)
	if len(other) != len(view) {
		t.Fatalf("second replay's normative event count = %d, want %d", len(other), len(view))
	}
	for i := range view {
		if view[i] != other[i] {
			t.Errorf("second replay diverged at normative event %d:\n got:  %s\n want: %s", i, other[i], view[i])
		}
	}
}

// replay2ndRun runs replay again under a distinct subtest name (so its RunID,
// derived from t.Name(), doesn't collide with the parent's) and returns its
// ConformanceView.
func replay2ndRun(t *testing.T, replay func(t *testing.T) []Event) []NormativeEvent {
	t.Helper()
	var out []NormativeEvent
	t.Run("second-replay", func(t *testing.T) {
		out = ConformanceView(replay(t))
	})
	return out
}

// TestMonotonicSeq covers both the happy path (a real journal's seq values,
// which are always exactly 1..N per appendEvent's increment-then-assign
// contract) and the failure modes a hand-built or corrupted Event slice could
// exhibit: a gap, a duplicate, and reordering.
func TestMonotonicSeq(t *testing.T) {
	valid := []Event{{Seq: 1}, {Seq: 2}, {Seq: 3}}
	if err := MonotonicSeq(valid); err != nil {
		t.Errorf("MonotonicSeq(valid) = %v, want nil", err)
	}
	if err := MonotonicSeq(nil); err != nil {
		t.Errorf("MonotonicSeq(nil) = %v, want nil", err)
	}

	cases := map[string][]Event{
		"gap":              {{Seq: 1}, {Seq: 3}},
		"duplicate":        {{Seq: 1}, {Seq: 1}},
		"reordered":        {{Seq: 2}, {Seq: 1}},
		"off-by-one start": {{Seq: 2}, {Seq: 3}},
	}
	for name, events := range cases {
		t.Run(name, func(t *testing.T) {
			if err := MonotonicSeq(events); err == nil {
				t.Errorf("MonotonicSeq(%s) = nil, want an error", name)
			}
		})
	}
}

// TestConformanceViewOnRealJournal exercises ConformanceView against a real
// Create/Append/Close/OpenRead round-trip (not hand-built Event literals),
// confirming the projection survives JSON marshal/unmarshal (e.g. the
// ExternalRef pointer and the AttemptClass string type) intact.
func TestConformanceViewOnRealJournal(t *testing.T) {
	run, root := newRun(t)
	art, err := run.RecordArtifact("plan.txt", []byte("step 1"))
	if err != nil {
		t.Fatalf("RecordArtifact: %v", err)
	}
	seq := []Event{
		{Type: EventStageStarted, Stage: "implement", Attempt: 1},
		{Type: EventStageFinished, Stage: "implement", Attempt: 1, Status: "success", Ref: &art},
		{Type: EventGateEvaluated, Gate: "review", Verdict: "pass"},
		{Type: EventRefTouched, ExternalRef: &ExternalRef{Provider: "github", Kind: "pr", ID: "42", URL: "https://x/42"}},
		{Type: EventRunFinished, Status: string(PhaseCompleted)},
	}
	for _, ev := range seq {
		if err := run.Append(ev); err != nil {
			t.Fatalf("Append %s: %v", ev.Type, err)
		}
	}
	if err := run.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rd, err := OpenRead(filepath.Join(root, testIdentity().RunID))
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	events, err := rd.Events()
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if err := MonotonicSeq(events); err != nil {
		t.Fatalf("MonotonicSeq: %v", err)
	}
	view := ConformanceView(events)
	// Create auto-appends run.started and RecordArtifact appends its own
	// artifact.recorded — both additional to seq, and (per IsConformanceNormative)
	// both conformance-normative, so the view is longer than seq itself.
	wantLen := len(seq) + 2
	if len(view) != wantLen {
		t.Fatalf("ConformanceView len = %d, want %d (run.started + artifact.recorded + len(seq), nothing here is excluded): %+v", len(view), wantLen, view)
	}

	var sawArtifact, sawRef bool
	for _, ne := range view {
		switch ne.Type {
		case EventArtifactRecorded:
			sawArtifact = true
			if ne.RefDigest != art.Digest {
				t.Errorf("artifact RefDigest = %q, want %q", ne.RefDigest, art.Digest)
			}
		case EventStageFinished:
			if ne.RefDigest != art.Digest {
				t.Errorf("stage.finished RefDigest = %q, want %q", ne.RefDigest, art.Digest)
			}
		case EventRefTouched:
			sawRef = true
			if ne.ExternalRefID != "42" || ne.ExternalRefProvider != "github" {
				t.Errorf("ref.touched projected = %+v, want provider=github id=42", ne)
			}
		}
	}
	if !sawArtifact {
		t.Error("expected an artifact.recorded event in the view")
	}
	if !sawRef {
		t.Error("expected a ref.touched event in the view")
	}
}
