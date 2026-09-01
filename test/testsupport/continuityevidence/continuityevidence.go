// Package continuityevidence records auditable, machine-readable evidence
// from the cross-substrate workspace-continuity harness (#4009).
//
// WHY EVIDENCE AND NOT JUST A PASSING TEST.
//
// #3803 and #3767 are release-acceptance line items on epic #3815 and its
// dispatch tracker #3828, and both were tracked for weeks against a baseline
// that could not be measured: `Goobernetes-Workflows`' pod-continuity-probe
// declared two MIXED arms (one pod stage, one self stage in the same lane),
// and a mixed lane is refused at boot by construction — selectEngineForEntry
// disqualifies a lane with any Self pin, and the runner-driven path cannot
// dispatch a pod pin. The arms never ran, so every "CURRENT-IMAGE
// EXPECTATION" recorded against them was a prediction.
//
// The replacement assertion has to live in-repo, next to the dispatcher and
// the worker provisioner. But an in-repo assertion is only usable for
// acceptance if a reader who was not present can tell WHAT was proven and on
// WHICH bytes — a green tick in a CI log is not a measurement. So each arm of
// the harness records its claim, its direction across the substrate boundary,
// whether it is a proof, an ablation or a refusal, and the concrete facts
// (commit SHAs, bundle digests, reconciliation outcomes) it turned on. The
// document that falls out is what a release record cites.
//
// The recorder deliberately holds no assertion logic. A harness that recorded
// evidence instead of asserting it would be the same defect as the probe it
// replaces: a claim nobody checked.
package continuityevidence

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// EnvDir names the directory a recorder writes its document to. Unset writes
// to the test's own temporary directory, which is discarded with the test —
// the document is logged in full either way, so CI output always carries the
// evidence even when nothing collected the file.
const EnvDir = "GOOBERS_CONTINUITY_EVIDENCE_DIR"

// Direction names which way across the substrate boundary an assertion runs.
// Cross-substrate continuity is not symmetric — one direction carries a pod's
// commits to the daemon-side mirror the worker cuts worktrees from, the other
// carries a mirror-side commit into a disposable pod checkout — so an
// assertion that does not say which one it exercised proves half of #3803.
const (
	// DirectionPodToSelf is #3803's headline: a self/worker-placed stage or
	// gate observing what a stage pod committed.
	DirectionPodToSelf = "pod->self"
	// DirectionSelfToPod is the reverse: a stage pod observing what a
	// self/worker-placed stage committed.
	DirectionSelfToPod = "self->pod"
	// DirectionSelection is a claim about WHICH publication a consumer
	// continues from, independent of the substrate that produced it (#3767).
	DirectionSelection = "selection"
)

// Kind classifies what an assertion establishes.
const (
	// KindProof is a positive assertion: the seam carried the work.
	KindProof = "proof"
	// KindAblation removes the seam and asserts the ORIGINAL defect
	// reproduces. Without one, a proof cannot distinguish "the carrier
	// works" from "the receiving substrate would have had the commits
	// anyway", which is exactly the mistake that let the mixed arms be
	// recorded as measurements.
	KindAblation = "ablation"
	// KindRefusal asserts a fail-closed arm: the seam refused loudly rather
	// than provisioning a workspace that silently omits earlier commits.
	KindRefusal = "refusal"
)

// Assertion is one recorded finding.
type Assertion struct {
	// Claim is the statement the harness checked, in the terms the issue
	// states it.
	Claim string `json:"claim"`
	// Kind is KindProof, KindAblation or KindRefusal.
	Kind string `json:"kind"`
	// Direction is one of the Direction constants.
	Direction string `json:"direction,omitempty"`
	// Refs are the issues this assertion answers ("#3803").
	Refs []string `json:"refs,omitempty"`
	// Facts are the concrete values the assertion turned on — commit SHAs,
	// bundle digests, reconciliation outcomes, refusal messages. Keys are
	// sorted by encoding/json, so the document is stable for a given run.
	Facts map[string]string `json:"facts,omitempty"`
}

// Document is the evidence one harness run produces.
type Document struct {
	// Probe names the harness (the replacement for the config probe that
	// could not run).
	Probe string `json:"probe"`
	// Issue is the tracker this harness discharges.
	Issue string `json:"issue"`
	// Substrates names the real components the harness composed, so a reader
	// can tell a seam test from a simulation.
	Substrates []string `json:"substrates,omitempty"`
	// RecordedAt is when the run finished, RFC3339.
	RecordedAt string `json:"recordedAt"`
	// Assertions are in the order the harness made them.
	Assertions []Assertion `json:"assertions"`
}

// TestingT is the subset of *testing.T a recorder uses.
//
// It is an interface for one reason: the recorder's own fail-closed rule — a
// harness that records NO evidence must fail — cannot be tested through a
// real *testing.T, because the failure it is supposed to raise would fail the
// test asserting it. #4009 exists because an unmeasurable probe was trusted;
// leaving this recorder's one behavioural rule unmeasured would repeat that
// on a smaller scale.
type TestingT interface {
	Helper()
	Cleanup(func())
	TempDir() string
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Logf(format string, args ...any)
}

// Recorder collects assertions and writes the document when the test ends.
type Recorder struct {
	t   TestingT
	doc Document
	dir string
}

// New starts a recorder for one harness. The document is written and logged
// on test cleanup; a harness that records nothing fails, because a probe that
// produces no evidence is the failure mode #4009 exists to close.
func New(t TestingT, probe, issue string, substrates ...string) *Recorder {
	t.Helper()
	dir := strings.TrimSpace(os.Getenv(EnvDir))
	if dir == "" {
		dir = t.TempDir()
	}
	r := &Recorder{
		t:   t,
		dir: dir,
		doc: Document{Probe: probe, Issue: issue, Substrates: substrates},
	}
	t.Cleanup(r.flush)
	return r
}

// Record appends one assertion.
func (r *Recorder) Record(a Assertion) {
	r.t.Helper()
	if a.Claim == "" || a.Kind == "" {
		r.t.Fatalf("continuityevidence: an assertion needs a claim and a kind, got %+v", a)
	}
	r.doc.Assertions = append(r.doc.Assertions, a)
}

// Path is where the document will be written.
func (r *Recorder) Path() string {
	return filepath.Join(r.dir, sanitize(r.doc.Probe)+".json")
}

func (r *Recorder) flush() {
	r.t.Helper()
	if len(r.doc.Assertions) == 0 {
		r.t.Errorf("continuityevidence: harness %q recorded no evidence", r.doc.Probe)
		return
	}
	r.doc.RecordedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(r.doc, "", "  ")
	if err != nil {
		r.t.Errorf("continuityevidence: marshal document: %v", err)
		return
	}
	data = append(data, '\n')
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		r.t.Errorf("continuityevidence: create evidence dir %s: %v", r.dir, err)
		return
	}
	if err := os.WriteFile(r.Path(), data, 0o644); err != nil {
		r.t.Errorf("continuityevidence: write %s: %v", r.Path(), err)
		return
	}
	r.t.Logf("cross-substrate continuity evidence (%s):\n%s", r.Path(), data)
}

// sanitize turns a probe name into a filename component.
func sanitize(name string) string {
	mapped := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, name)
	if mapped == "" {
		return "evidence"
	}
	return mapped
}
