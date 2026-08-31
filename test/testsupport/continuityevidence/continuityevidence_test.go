package continuityevidence_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/continuityevidence"
)

// fakeT stands in for *testing.T so the recorder's own failure rule can be
// asserted without the assertion itself failing (see TestingT's doc).
type fakeT struct {
	tb       testing.TB
	dir      string
	cleanups []func()
	errs     []string
	fatals   []string
	logs     []string
}

func newFakeT(tb testing.TB) *fakeT {
	tb.Helper()
	return &fakeT{tb: tb, dir: tb.TempDir()}
}

func (f *fakeT) Helper()           {}
func (f *fakeT) Cleanup(fn func()) { f.cleanups = append(f.cleanups, fn) }
func (f *fakeT) TempDir() string   { return f.dir }

func (f *fakeT) Errorf(format string, a ...any) { f.errs = append(f.errs, fmt.Sprintf(format, a...)) }
func (f *fakeT) Logf(format string, a ...any)   { f.logs = append(f.logs, fmt.Sprintf(format, a...)) }

// Fatalf records instead of aborting: the recorder calls it on an invalid
// assertion, and the fake has no goroutine to stop.
func (f *fakeT) Fatalf(format string, a ...any) {
	f.fatals = append(f.fatals, fmt.Sprintf(format, a...))
}

// run executes the cleanups the recorder registered, in the order testing
// would, so a test can observe what flush did.
func (f *fakeT) run() {
	for i := len(f.cleanups) - 1; i >= 0; i-- {
		f.cleanups[i]()
	}
}

// TestRecordedEvidenceIsWrittenAndComplete is the recorder's contract: what a
// harness records is what a release reviewer reads, so the document must
// round-trip through JSON with every field a reviewer needs to tell a proof
// from an ablation.
func TestRecordedEvidenceIsWrittenAndComplete(t *testing.T) {
	fake := newFakeT(t)
	rec := continuityevidence.New(fake, "probe/name", "#4009", "internal/workerhost", "cmd/goobers")
	rec.Record(continuityevidence.Assertion{
		Claim:     "a self stage sees a pod's commits",
		Kind:      continuityevidence.KindProof,
		Direction: continuityevidence.DirectionPodToSelf,
		Refs:      []string{"#3803"},
		Facts:     map[string]string{"digest": "sha256:abc"},
	})
	rec.Record(continuityevidence.Assertion{
		Claim:     "without the delta the stage provisions at base",
		Kind:      continuityevidence.KindAblation,
		Direction: continuityevidence.DirectionSelfToPod,
	})
	fake.run()

	if len(fake.errs) != 0 || len(fake.fatals) != 0 {
		t.Fatalf("a complete recording must not fail: errs=%v fatals=%v", fake.errs, fake.fatals)
	}
	// The filename must be usable as a path component: probe names carry
	// slashes, and a document written to a directory that does not exist is
	// evidence nobody can cite.
	if base := filepath.Base(rec.Path()); base != "probe-name.json" {
		t.Errorf("Path() base = %q, want probe-name.json", base)
	}
	data, err := os.ReadFile(rec.Path())
	if err != nil {
		t.Fatalf("read evidence document: %v", err)
	}
	var doc continuityevidence.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("evidence must be machine-readable: %v", err)
	}
	if doc.Probe != "probe/name" || doc.Issue != "#4009" {
		t.Errorf("document identity lost: %+v", doc)
	}
	if len(doc.Substrates) != 2 {
		t.Errorf("substrates = %v; a reader cannot tell a seam test from a simulation without them", doc.Substrates)
	}
	if doc.RecordedAt == "" {
		t.Error("recordedAt must be stamped, or the evidence cannot be tied to a run")
	}
	if len(doc.Assertions) != 2 {
		t.Fatalf("assertions = %d, want 2 in recorded order", len(doc.Assertions))
	}
	if doc.Assertions[0].Kind != continuityevidence.KindProof ||
		doc.Assertions[1].Kind != continuityevidence.KindAblation {
		t.Errorf("assertion order or kind lost: %+v", doc.Assertions)
	}
	if doc.Assertions[0].Facts["digest"] != "sha256:abc" {
		t.Errorf("facts dropped: %+v", doc.Assertions[0])
	}
	// The document is logged in full, so CI output carries the evidence even
	// when no collector picked the file up.
	if len(fake.logs) != 1 || !strings.Contains(fake.logs[0], "sha256:abc") {
		t.Errorf("the document must be logged in full: %v", fake.logs)
	}
}

// TestSilentHarnessFails is the recorder's own ablation. #4009's root cause is
// a probe whose arms never ran while its status still read green; a recorder
// that stayed silent when a harness asserted nothing would reproduce exactly
// that at the evidence layer.
func TestSilentHarnessFails(t *testing.T) {
	fake := newFakeT(t)
	rec := continuityevidence.New(fake, "silent", "#4009")
	fake.run()

	if len(fake.errs) == 0 {
		t.Fatal("a harness that records no evidence must fail, not pass quietly")
	}
	if !strings.Contains(fake.errs[0], "no evidence") {
		t.Errorf("failure must say what is missing: %q", fake.errs[0])
	}
	if _, err := os.Stat(rec.Path()); !os.IsNotExist(err) {
		t.Errorf("an empty document must not be written: stat err = %v", err)
	}
}

// TestIncompleteAssertionIsRejected keeps the document readable: an assertion
// with no claim or no kind is a row a reviewer cannot act on.
func TestIncompleteAssertionIsRejected(t *testing.T) {
	for name, a := range map[string]continuityevidence.Assertion{
		"no claim": {Kind: continuityevidence.KindProof},
		"no kind":  {Claim: "something happened"},
	} {
		t.Run(name, func(t *testing.T) {
			fake := newFakeT(t)
			continuityevidence.New(fake, "incomplete", "#4009").Record(a)
			if len(fake.fatals) == 0 {
				t.Error("an assertion missing its claim or kind must be rejected")
			}
		})
	}
}

// TestEvidenceDirEnvIsHonoured is what makes the evidence collectable: CI
// points EnvDir at a directory it archives, and the harness must write there
// instead of a temporary directory the run discards.
func TestEvidenceDirEnvIsHonoured(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "collected")
	t.Setenv(continuityevidence.EnvDir, dir)

	fake := newFakeT(t)
	rec := continuityevidence.New(fake, "collected", "#4009")
	rec.Record(continuityevidence.Assertion{Claim: "c", Kind: continuityevidence.KindRefusal})
	fake.run()

	if filepath.Dir(rec.Path()) != dir {
		t.Fatalf("Path() = %s, want a document under %s", rec.Path(), dir)
	}
	// The directory is created on demand: a CI job that exports the variable
	// without pre-creating the path still collects its evidence.
	if _, err := os.Stat(rec.Path()); err != nil {
		t.Fatalf("evidence must be written to the collected directory: %v", err)
	}
	if len(fake.errs) != 0 {
		t.Errorf("unexpected failures: %v", fake.errs)
	}
}
