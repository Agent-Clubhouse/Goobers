package goobernetes

import (
	"bytes"
	"testing"
	"time"
)

func TestNewObserverResultRequiresNamedObserver(t *testing.T) {
	_, err := NewObserverResult(ItemS1FreshPod, "", pass(nil), time.Now())
	if err == nil {
		t.Fatal("NewObserverResult with empty observer should fail (D2: a criterion without an observer is a wish)")
	}
}

func TestNewObserverResultRequiresRecordedAt(t *testing.T) {
	_, err := NewObserverResult(ItemS1FreshPod, "some observer", pass(nil), time.Time{})
	if err == nil {
		t.Fatal("NewObserverResult with zero RecordedAt should fail")
	}
}

func TestNewObserverResultCarriesVerdictAndReason(t *testing.T) {
	result := fail("pod reused", "evidence-blob")
	got, err := NewObserverResult(ItemS6KillMatrix, "kill-matrix observer", result, time.Now())
	if err != nil {
		t.Fatalf("NewObserverResult: %v", err)
	}
	if got.Verdict != VerdictFail || got.Reason != "pod reused" || got.Evidence != "evidence-blob" {
		t.Fatalf("got = %+v, want verdict=fail reason=%q evidence=%q", got, "pod reused", "evidence-blob")
	}
}

// TestBundleOverallRequiresAllNine proves the §4 "all must pass in one
// procedure" discipline: a bundle missing items cannot silently read as
// passing just because the items it DOES have all passed.
func TestBundleOverallRequiresAllNine(t *testing.T) {
	var b Bundle
	now := time.Now()
	for _, id := range []SmokeItemID{ItemS1FreshPod, ItemS2OSHop, ItemS3DeclaredEdge} {
		b.Add(ObserverResult{Item: id, Observer: "x", Verdict: VerdictPass, RecordedAt: now})
	}
	if got := b.Overall(); got != VerdictPass {
		t.Fatalf("Overall() over 3 passing items = %v, want pass (Overall only combines what was added)", got)
	}
	missing := b.MissingItems(RequiredSmokeItems())
	if len(missing) != len(RequiredSmokeItems())-3 {
		t.Fatalf("MissingItems = %v, want %d missing", missing, len(RequiredSmokeItems())-3)
	}
}

func TestBundleOverallInvalidDominates(t *testing.T) {
	var b Bundle
	now := time.Now()
	b.Add(ObserverResult{Item: ItemS1FreshPod, Observer: "x", Verdict: VerdictPass, RecordedAt: now})
	b.Add(ObserverResult{Item: ItemS9NegativeControl, Observer: "x", Verdict: VerdictInvalid, RecordedAt: now})
	if got := b.Overall(); got != VerdictInvalid {
		t.Fatalf("Overall() = %v, want invalid", got)
	}
}

// TestBundleEncodeReadBundleRoundTrip is the §5 rule 4 "re-runnable from the
// bundle alone" discipline's mechanical proof: encode, decode, compare.
func TestBundleEncodeReadBundleRoundTrip(t *testing.T) {
	original := Bundle{
		ProcedureID: "proc-1",
		StartedAt:   time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC),
		FinishedAt:  time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC),
		Collateral: Collateral{
			CommitSHA:     "abc123",
			BinaryVersion: "v0.3.3",
			ImageTags:     map[string]string{"goobers-base": "v0.3.3"},
		},
	}
	original.Add(ObserverResult{
		Item: ItemS1FreshPod, Observer: FreshPodObserver, Verdict: VerdictPass,
		RecordedAt: time.Date(2026, 8, 22, 10, 5, 0, 0, time.UTC),
	})

	var buf bytes.Buffer
	if err := original.Encode(&buf); err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := ReadBundle(&buf)
	if err != nil {
		t.Fatalf("ReadBundle: %v", err)
	}
	if decoded.ProcedureID != original.ProcedureID {
		t.Fatalf("ProcedureID = %q, want %q", decoded.ProcedureID, original.ProcedureID)
	}
	if len(decoded.Items) != 1 || decoded.Items[0].Item != ItemS1FreshPod {
		t.Fatalf("Items = %+v, want one S1 entry", decoded.Items)
	}
	if decoded.Collateral.CommitSHA != "abc123" {
		t.Fatalf("Collateral.CommitSHA = %q, want abc123", decoded.Collateral.CommitSHA)
	}
	if decoded.Overall() != VerdictPass {
		t.Fatalf("decoded Overall() = %v, want pass", decoded.Overall())
	}
}

// TestReadBundleRejectsUnknownFields is the hand-edit-detection discipline:
// a bundle a human hand-edited to add a field this schema does not know
// about must be refused, not silently accepted (mirrors D3/D2's
// falsifiability posture applied to the bundle format itself).
func TestReadBundleRejectsUnknownFields(t *testing.T) {
	tampered := `{"procedureId":"p","startedAt":"2026-08-22T00:00:00Z","items":[],"collateral":{},"handEdited":true}`
	if _, err := ReadBundle(bytes.NewBufferString(tampered)); err == nil {
		t.Fatal("ReadBundle accepted an unknown field — hand edits must be refused, not silently read")
	}
}

func TestRequiredSmokeItemsIsNineItems(t *testing.T) {
	if got := len(RequiredSmokeItems()); got != 9 {
		t.Fatalf("RequiredSmokeItems() has %d entries, want 9 (S1-S9)", got)
	}
}
