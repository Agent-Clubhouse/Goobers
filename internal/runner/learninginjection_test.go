package runner

// Coverage for the SHARED half of learning-episode injection (#3913, plan item
// E10) and for the one route that stays runner-only.
//
// internal/engine's gate retry arm calls BuildLearningEpisode,
// LearningEpisodeAnnotation, LearningSourceEvent and the two name builders.
// That makes them a contract between two runners rather than internal helpers
// of this one: an edit here changes the engine's behaviour too, and — because
// the episode is content-addressed — changes the ID every cross-run learning
// consumer correlates on. These tests state the properties that sharing is
// supposed to buy, which the parity harness cannot: parity compares two walks
// of the SAME helpers, so a helper that drifted would drift identically on both
// sides and every row would stay green.

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/journal"
)

func learningEpisodeTestInput() LearningEpisodeInput {
	return LearningEpisodeInput{
		RunID:          "run-1",
		Workflow:       "web",
		WorkflowDigest: "sha256:wf",
		Gate:           "review",
		Stage:          "implement",
		SourceSeq:      4,
		SourceAttempt:  1,
		SourceResult: apiv1.ResultEnvelope{
			Status:  apiv1.ResultFailure,
			Summary: "3 tests failed",
			Error:   &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "# tests failed"},
		},
	}
}

// The names both runners derive must encode the SOURCE event's sequence, and
// the artifact name and pointer name must agree about it.
//
// This is what makes an injected pointer traceable: "learning.episode[4]" in a
// stage's envelope and "learning/episode-review-4.json" in the journal are the
// same claim about the same failure. A builder that drifted — a different
// separator, a gate-only name, an attempt number instead of a sequence — would
// break every consumer that joins the two, and would do it identically on both
// runners, which is exactly the class of bug parity cannot catch.
func TestLearningEpisodeNamesEncodeTheSourceSequence(t *testing.T) {
	if got, want := LearningEpisodeArtifactName("review", 4), "learning/episode-review-4.json"; got != want {
		t.Fatalf("artifact name = %q, want %q", got, want)
	}
	if got, want := LearningEpisodePointerName(4), "learning.episode[4]"; got != want {
		t.Fatalf("pointer name = %q, want %q", got, want)
	}
	// Both prefixes are what consumers (and the parity harness) match on to
	// find an injection without knowing which event it corrects.
	if !strings.HasPrefix(LearningEpisodeArtifactName("review", 4), "learning/episode-") {
		t.Fatal(`artifact name does not carry the "learning/episode-" prefix consumers match on`)
	}
	if !strings.HasPrefix(LearningEpisodePointerName(4), "learning.episode[") {
		t.Fatal(`pointer name does not carry the "learning.episode[" prefix consumers match on`)
	}
}

// The same input must produce byte-identical episode JSON, and therefore the
// same id and signature, every time.
//
// The engine records this artifact from inside a replayed workflow, so this is
// not a style preference: an unstable encoding there is a nondeterminism panic
// that wedges a live run. Building the episode twice and comparing the bytes
// catches map-ordered encoding, time reads, or any other impurity at the point
// it is introduced rather than on a production replay.
func TestBuildLearningEpisodeIsDeterministic(t *testing.T) {
	first := BuildLearningEpisode(learningEpisodeTestInput())
	second := BuildLearningEpisode(learningEpisodeTestInput())
	// Both runners marshal the episode with encoding/json and address the
	// bytes, so comparing the encodings is comparing what actually gets
	// digested and named.
	firstData, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("encode episode: %v", err)
	}
	secondData, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("encode episode (second): %v", err)
	}
	if string(firstData) != string(secondData) {
		t.Fatalf("episode encoding is not stable:\n  first:  %s\n  second: %s", firstData, secondData)
	}
	if first.ID != second.ID || first.Signature != second.Signature {
		t.Fatalf("episode identity is not stable: (%s, %s) vs (%s, %s)",
			first.ID, first.Signature, second.ID, second.Signature)
	}
	if first.ID == "" || first.Signature == "" {
		t.Fatalf("episode identity is empty: id=%q signature=%q", first.ID, first.Signature)
	}
	// And the id is genuinely content-addressed over the episode rather than
	// over its identity fields alone: a different correction produces a
	// different id, so consumers keyed on it do not collapse two failures.
	other := learningEpisodeTestInput()
	other.SourceResult.Error = &apiv1.ErrorInfo{Code: "nonzero_exit", Message: "a different failure"}
	changed := BuildLearningEpisode(other)
	if changed.ID == first.ID {
		t.Fatalf("two different failures produced the same episode id %q", first.ID)
	}
}

// A deterministic failure — no reviewer, no verdict — must still produce a
// usable episode.
//
// This is the property that makes the injection lane-agnostic, and therefore
// the reason #3913 is the GENERIC retry arm rather than part of #3882's
// verdict-gated work. If the non-verdict fallback ever stopped synthesizing a
// finding, the port would quietly become reviewer-only and every deterministic
// repass would lose its correction feedback without any test failing.
func TestBuildLearningEpisodeFallsBackWithoutAVerdict(t *testing.T) {
	episode := BuildLearningEpisode(learningEpisodeTestInput())
	if len(episode.Findings) != 1 {
		t.Fatalf("episode carries %d findings, want 1 synthesized from the failure", len(episode.Findings))
	}
	if got := episode.Findings[0].Message; got != "# tests failed" {
		t.Fatalf("synthesized finding message = %q, want the failure's error message", got)
	}
	if episode.Findings[0].Location != "implement" {
		t.Fatalf("synthesized finding location = %q, want the failing stage", episode.Findings[0].Location)
	}
	if episode.Classification != apiv1.LearningValidation {
		t.Fatalf("classification = %q, want %q", episode.Classification, apiv1.LearningValidation)
	}
	if episode.CorrectionFeedback != "# tests failed" {
		t.Fatalf("correction feedback = %q, want the error message (the runner's precedence: rationale, "+
			"then error message, then summary)", episode.CorrectionFeedback)
	}
	if episode.NextAttempt != episode.SourceAttempt+1 {
		t.Fatalf("nextAttempt = %d, want sourceAttempt+1 (%d)", episode.NextAttempt, episode.SourceAttempt+1)
	}

	// The last-resort message: a failure carrying nothing at all still yields a
	// finding rather than an empty one.
	bare := learningEpisodeTestInput()
	bare.SourceResult = apiv1.ResultEnvelope{Status: apiv1.ResultFailure}
	empty := BuildLearningEpisode(bare)
	if len(empty.Findings) != 1 || empty.Findings[0].Message != "validation failed" {
		t.Fatalf("bare failure produced findings %+v, want one \"validation failed\" finding",
			empty.Findings)
	}
}

// With a reviewer verdict, the verdict's findings replace the synthesized one,
// the rationale becomes the correction feedback, and the verdict POINTER leads
// the evidence list.
//
// The evidence order is the subtle part and the reason the engine passes its
// verdict pointer into the shared builder rather than appending it afterwards:
// evidence is inside the content-addressed episode, so a runner that ordered it
// differently would produce a different id for the same failure — silently.
func TestBuildLearningEpisodeUsesTheVerdictArm(t *testing.T) {
	in := learningEpisodeTestInput()
	in.Verdict = &apiv1.Verdict{
		Decision:  apiv1.VerdictFail,
		Rationale: "the reviewer explained why",
		Findings: []apiv1.Finding{{
			Severity: apiv1.SeverityError, Message: "missing test coverage", Location: "implement",
			LearningClassification: apiv1.LearningValidation,
		}},
	}
	in.VerdictPointer = &apiv1.ContextPointer{
		Name:      "review.verdict",
		Integrity: apiv1.IntegrityDerived,
		Artifact:  &apiv1.ArtifactPointer{Path: "artifacts/sha256/aa/bb", Digest: "sha256:aabb"},
	}
	episode := BuildLearningEpisode(in)
	if len(episode.Findings) != 1 || episode.Findings[0].Message != "missing test coverage" {
		t.Fatalf("episode findings = %+v, want the verdict's findings", episode.Findings)
	}
	if episode.Findings[0].ID == "" {
		t.Fatal("verdict findings must be normalized (identity assigned) before they enter the episode")
	}
	if episode.CorrectionFeedback != "the reviewer explained why" {
		t.Fatalf("correction feedback = %q, want the verdict rationale", episode.CorrectionFeedback)
	}
	if len(episode.Evidence) == 0 {
		t.Fatal("episode carries no evidence")
	}
	if got := episode.Evidence[0].Digest; got != "sha256:aabb" {
		t.Fatalf("evidence[0] digest = %q, want the verdict artifact's — the verdict pointer leads the "+
			"evidence list, and evidence is inside the content-addressed episode", got)
	}
}

// recordLearningInjection must record the artifact, append the annotation and
// hand back a DERIVED pointer that carries the ref its own writer produced.
//
// The grade is the behaviour, not decoration: produced integrity is the floor
// of a stage's inputs, so this pointer is what downgrades the repassed stage's
// stage.finished from trusted to derived, and minimumIntegrity is admission
// control. A pointer built at any stronger grade would let a repassed stage
// pass an admission check the engine's own port (which hardcodes derived)
// refuses — a divergence no single-runner test would catch.
//
// The annotation is asserted in the same walk because the two must stay
// consistent: it is attributed to the stage being RE-ENTERED at its NEXT
// attempt, and it names the artifact whose digest the pointer carries. Both
// runners append this exact event, so a stage/attempt drift between them shows
// up in the journal as an injection belonging to a different invocation than
// the one it is evidence for.
func TestRecordLearningInjectionPointerAndAnnotation(t *testing.T) {
	const runID = "run-learning"
	runsDir := filepath.Join(t.TempDir(), "runs")
	run, err := journal.Create(runsDir, journal.RunIdentity{
		RunID: runID, Workflow: "fixture", WorkflowVersion: 1,
		WorkflowDigest: "sha256:workflow", GooberDigest: "sha256:goober",
		Gaggle: "acme-web", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil)
	if err != nil {
		t.Fatalf("create journal: %v", err)
	}
	t.Cleanup(func() { _ = run.Close() })

	// A deterministic failure — no reviewer, no verdict — so this covers the
	// GENERIC retry arm rather than the verdict-gated one.
	in := StartInput{RunID: runID, Machine: fixtureMachine(t), Gaggle: "acme-web", GooberDigest: "sha256:goober"}
	gr := gate.Result{Attempt: 1}
	source := learningEpisodeTestInput().SourceResult
	pointer, err := recordLearningInjection(run, in, "review", "implement", gr, "implement", source, nil)
	if err != nil {
		t.Fatalf("record learning injection: %v", err)
	}
	if pointer == nil {
		t.Fatal("recordLearningInjection returned no pointer; the repass would be dispatched without its correction")
	}
	if !strings.HasPrefix(pointer.Name, "learning.episode[") {
		t.Fatalf("pointer name = %q, want a learning.episode[<seq>] name", pointer.Name)
	}
	if pointer.Integrity != apiv1.IntegrityDerived {
		t.Fatalf("pointer integrity = %q, want %q — the engine's port hardcodes derived, so a local writer "+
			"producing anything stronger is a silent conformance divergence",
			pointer.Integrity, apiv1.IntegrityDerived)
	}
	if pointer.Artifact == nil || pointer.Artifact.Digest == "" ||
		pointer.Artifact.Integrity != apiv1.IntegrityDerived ||
		pointer.Artifact.MediaType != "application/json" {
		t.Fatalf("pointer artifact = %+v, want the recorded ref at derived integrity", pointer.Artifact)
	}

	if err := run.Close(); err != nil {
		t.Fatalf("close journal: %v", err)
	}
	events := readJournalEvents(t, filepath.Join(runsDir, runID))
	var annotation journal.Event
	for _, ev := range events {
		if ev.Type == journal.EventRunnerAnnotation && strings.HasPrefix(ev.Name, "learning/episode-") {
			annotation = ev
		}
	}
	if annotation.Name == "" {
		t.Fatalf("no learning episode annotation in the journal:\n%+v", events)
	}
	if annotation.Stage != "implement" || annotation.Attempt != gr.Attempt+1 {
		t.Fatalf("annotation attributed to %s/%d, want implement/%d — the injection is evidence for the "+
			"invocation it feeds, not the one that failed",
			annotation.Stage, annotation.Attempt, gr.Attempt+1)
	}
	if got, _ := annotation.Runner["kind"].(string); got != LearningEpisodeInjectedKind {
		t.Fatalf("annotation kind = %q, want %q", got, LearningEpisodeInjectedKind)
	}
	if got, _ := annotation.Runner["episodeDigest"].(string); got != pointer.Artifact.Digest {
		t.Fatalf("annotation episodeDigest = %q but the pointer carries %q; the two must name the same "+
			"bytes or a reader cannot join them", got, pointer.Artifact.Digest)
	}
	for _, key := range []string{
		"episodeId", "sourceRunId", "sourceSeq", "gate", "target", "sourceAttempt", "nextAttempt",
		"signature", "classification", "recommendedAction", "findingIdentities", "correctionFeedback",
		"episodePath", "episodeDigest",
	} {
		if _, ok := annotation.Runner[key]; !ok {
			t.Errorf("annotation is missing %q; both runners read this payload", key)
		}
	}
	// The payload must survive a JSON round trip, because the engine's copy
	// reaches readers through a Temporal query rather than through memory.
	encoded, err := json.Marshal(annotation.Runner)
	if err != nil {
		t.Fatalf("encode annotation: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode annotation: %v", err)
	}
	if decoded["episodeId"] == "" || decoded["episodeId"] == nil {
		t.Fatalf("episodeId did not survive encoding: %v", decoded["episodeId"])
	}
}

// readJournalEvents reads a live run's appended events back off disk.
func readJournalEvents(t *testing.T, dir string) []journal.Event {
	t.Helper()
	reader, err := journal.OpenRead(dir)
	if err != nil {
		t.Fatalf("open journal for read: %v", err)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("read journal events: %v", err)
	}
	return events
}

// The runner's retry arm scopes the injected pointer to the ACTIVE BRANCH when
// one is running, rather than to the run-level pointer set.
//
// This is the route the engine deliberately does not port — spec.parallels is
// refused at start there (asserted from the other side by
// TestLearningEpisodeParallelRouteIsRunnerOnly in internal/engine). Pinning the
// scoping here is what makes that split safe to state: a branch-scoped pointer
// must not leak into sibling branches, because two branches repassing on
// different failures would otherwise each be dispatched with the other's
// correction.
func TestLearningEpisodePointerIsBranchScoped(t *testing.T) {
	p := newParallelExec(apiv1.Parallel{
		Name: "fan",
		Branches: []apiv1.Branch{
			{Name: "security", Start: "a"},
			{Name: "perf", Start: "b"},
		},
	})
	pointer := apiv1.ContextPointer{
		Name:      LearningEpisodePointerName(4),
		Integrity: apiv1.IntegrityDerived,
		Artifact: &apiv1.ArtifactPointer{
			Path: "artifacts/sha256/aa/bb", Digest: "sha256:aabb",
			MediaType: "application/json", Integrity: apiv1.IntegrityDerived,
		},
	}

	root := []apiv1.ContextPointer{{Name: "run.level", Integrity: apiv1.IntegrityTrusted}}
	p.active = 0
	p.recordCurrentPointer(pointer)

	inBranch := p.currentPointers(root)
	if !containsPointer(inBranch, pointer.Name) {
		t.Fatalf("first branch pointers = %v, want the injected %q", pointerNames(inBranch), pointer.Name)
	}
	if !containsPointer(inBranch, "run.level") {
		t.Fatalf("first branch pointers = %v, want the run-level pointers too", pointerNames(inBranch))
	}
	p.active = 1
	if sibling := p.currentPointers(root); containsPointer(sibling, pointer.Name) {
		t.Fatalf("sibling branch pointers = %v, but %q was injected into the first branch; a branch-scoped "+
			"correction must not be dispatched to a sibling", pointerNames(sibling), pointer.Name)
	}
	// The run-level set the walk carries outside the parallel is untouched.
	if containsPointer(root, pointer.Name) {
		t.Fatalf("run-level pointers = %v, but %q was injected inside a branch",
			pointerNames(root), pointer.Name)
	}
}

func containsPointer(pointers []apiv1.ContextPointer, name string) bool {
	for _, p := range pointers {
		if p.Name == name {
			return true
		}
	}
	return false
}

func pointerNames(pointers []apiv1.ContextPointer) []string {
	out := make([]string, 0, len(pointers))
	for _, p := range pointers {
		out = append(out, p.Name)
	}
	return out
}
