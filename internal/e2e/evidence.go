package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// SmokeItemID identifies one falsifiable criterion of
// goobernetes-smoke.md §4 (S1-S9) or an goobernetes-architecture.md §11
// acceptance item folded into the same procedure (§11 items 5-8 are S8, S2,
// S6, and the capability-gap check respectively — see each assertion
// helper's doc comment for the exact cross-reference).
type SmokeItemID string

// The closed set of smoke items this bundle can carry a verdict for.
// goobernetes-smoke.md §4 defines S1-S9; item names below quote its section
// headers verbatim so a bundle reader can find the prose without translating
// an id.
const (
	ItemS1FreshPod               SmokeItemID = "S1"       // Fresh pod per stage attempt, never reused
	ItemS2OSHop                  SmokeItemID = "S2"       // OS hop within one run
	ItemS3DeclaredEdge           SmokeItemID = "S3"       // Declared-edge repo handoff across nodes
	ItemS4ArtifactAcrossNode     SmokeItemID = "S4"       // Artifact materialization across nodes
	ItemS5RepassAcrossNode       SmokeItemID = "S5"       // Repass across nodes with the gate verdict pointer
	ItemS6KillMatrix             SmokeItemID = "S6"       // Kill matrix: pod-kill and node-kill, per stage class
	ItemS7WriteAPI               SmokeItemID = "S7"       // Triggers and HITL through the write API
	ItemS8LiveVisibility         SmokeItemID = "S8"       // Live stage-transition visibility mid-run
	ItemS9NegativeControl        SmokeItemID = "S9"       // network:allowlist proven by a negative control
	ItemArch11Item8CapabilityGap SmokeItemID = "ARCH11-8" // architecture §11 item 8: boot proportionality / capability-gap refusal
)

// ObserverResult is one smoke item's outcome: the item id, its NAMED
// OBSERVER (goobernetes-smoke.md D2: "a criterion without an observer is a
// wish" — this field is required to be non-empty by NewObserverResult), the
// observed evidence, and the pass/fail/invalid verdict ClassifyItem produced.
// This is the artifact item 1 of the #3517 task asks for: a typed structure
// capturing, per S1-S9 item, exactly this tuple.
type ObserverResult struct {
	Item SmokeItemID `json:"item"`
	// Observer names the record that proves the criterion — e.g. "runner.*
	// journal events + StageAttempt.Placement fields" for S1. Mirrors the
	// smoke doc's own "**Observer:**" paragraph for the item so a bundle
	// reader can find the corresponding prose.
	Observer string `json:"observer"`
	// Evidence is the raw observed data (JSON-serializable): the assertion
	// helper's AssertionResult.Evidence, or, for a topology-pending item,
	// whatever collateral a live run captured. Left untyped because each
	// item's evidence shape differs (placement lists, exit-code triples,
	// journal excerpts, ...); the schema constraint is on the envelope
	// (Item/Observer/Verdict/Reason), not on this payload.
	Evidence any     `json:"evidence,omitempty"`
	Verdict  Verdict `json:"verdict"`
	// Reason explains a non-pass verdict (empty on a clean pass, per
	// AssertionResult's convention).
	Reason string `json:"reason,omitempty"`
	// RecordedAt is when this result was captured, independent of the
	// bundle's own StartedAt/FinishedAt (§5 rule 3's "evidence bundle on
	// completion" is one deliverable; individual items are typically
	// recorded as the procedure runs, before that final bundle write).
	RecordedAt time.Time `json:"recordedAt"`
}

// NewObserverResult builds an ObserverResult from an AssertionResult,
// refusing to construct one with no named observer (D2) or a zero
// RecordedAt (an unrecorded verdict is unfalsifiable — an item without a
// timestamp can never be checked against the run's own terminal-event
// ordering, which S8 in particular depends on).
func NewObserverResult(item SmokeItemID, observer string, result AssertionResult, recordedAt time.Time) (ObserverResult, error) {
	if observer == "" {
		return ObserverResult{}, fmt.Errorf("goobernetes: item %s has no named observer (goobernetes-smoke.md D2: a criterion without an observer is a wish)", item)
	}
	if recordedAt.IsZero() {
		return ObserverResult{}, fmt.Errorf("goobernetes: item %s result has no RecordedAt", item)
	}
	return ObserverResult{
		Item:       item,
		Observer:   observer,
		Evidence:   result.Evidence,
		Verdict:    result.Verdict,
		Reason:     result.Detail,
		RecordedAt: recordedAt,
	}, nil
}

// Collateral is the rest of what goobernetes-smoke.md §5 rule 3 requires an
// evidence bundle to hold, beyond the per-item ObserverResults: "applied DSL
// + inventory, all run journals (events.jsonl), the injection records (D5),
// the write-API request log for S7, the S8 capture, the S9 probe output,
// image tags and binary version, cluster/node inventory." Every field is a
// reference (a path, a digest, an opaque blob) rather than a parsed
// structure, because the exact shape of each artifact is topology-specific
// (which files exist, how the cluster names nodes) and is filled in when the
// live procedure runs; this bundle's job is to hold a NAMED SLOT for each
// one so nothing required by §5 rule 3 is silently omitted.
type Collateral struct {
	// AppliedDSLPath and InventoryPath point at the applied workflow YAML and
	// the resolved runners: inventory the procedure ran against.
	AppliedDSLPath string `json:"appliedDSLPath,omitempty"`
	InventoryPath  string `json:"inventoryPath,omitempty"`
	// RunJournalPaths lists every run's events.jsonl captured (§5 rule 3).
	RunJournalPaths []string `json:"runJournalPaths,omitempty"`
	// InjectionRecords is S6's per-cell record (D5: "each injection is
	// itself recorded in the evidence bundle").
	InjectionRecords []CellInjectionRecord `json:"injectionRecords,omitempty"`
	// WriteAPIRequestLogPath is S7's observer collateral.
	WriteAPIRequestLogPath string `json:"writeAPIRequestLogPath,omitempty"`
	// S8CapturePath is the live-visibility capture (SSE event log or
	// timestamped portal screenshot — §8 open point 3 leaves the form open;
	// this field is form-agnostic, a path to whichever was chosen).
	S8CapturePath string `json:"s8CapturePath,omitempty"`
	// S9ProbeOutputPath is the raw curl/probe transcript backing the
	// negative-control triple.
	S9ProbeOutputPath string `json:"s9ProbeOutputPath,omitempty"`
	// ImageTags maps image name to the tag actually pulled (decision D8:
	// tags = binary version).
	ImageTags map[string]string `json:"imageTags,omitempty"`
	// BinaryVersion and CommitSHA record what ran the procedure. Per
	// goobernetes-smoke.md §1, the smoke "runs off main-commit SHAs, no
	// tagged releases" — CommitSHA is the one that must always be present;
	// BinaryVersion is whatever `goobers version` reports for it.
	BinaryVersion string `json:"binaryVersion,omitempty"`
	CommitSHA     string `json:"commitSHA,omitempty"`
	// ClusterNodeInventoryPath is a dump of the cluster's actual node list at
	// procedure time (name/OS), for cross-checking against Placement.Node
	// values recorded in the run journals.
	ClusterNodeInventoryPath string `json:"clusterNodeInventoryPath,omitempty"`
}

// Bundle is the durable evidence bundle goobernetes-smoke.md §5 rules 3-4
// describe: "captured on fail and invalid before teardown" (D4), one per
// procedure, "re-runnable from the bundle alone" (§5 rule 4). This is the
// artifact a live smoke run fills; everything in this package that produces
// an ObserverResult is meant to be appended to one.
type Bundle struct {
	// ProcedureID names one run of the whole smoke procedure (not a single
	// workflow run — §4: "All must pass in one procedure... a small number
	// of runs on one cluster, one evidence bundle").
	ProcedureID string           `json:"procedureId"`
	StartedAt   time.Time        `json:"startedAt"`
	FinishedAt  time.Time        `json:"finishedAt,omitempty"`
	Items       []ObserverResult `json:"items"`
	Collateral  Collateral       `json:"collateral"`
}

// Add appends result and returns the bundle for chaining. It never mutates
// existing entries — a bundle records history, it does not overwrite a prior
// item's verdict (a re-run of the same item is a NEW ObserverResult, not a
// silent replacement, so a bundle reader can see that it was retried).
func (b *Bundle) Add(result ObserverResult) *Bundle {
	b.Items = append(b.Items, result)
	return b
}

// Overall is the bundle's OverallVerdict over every recorded item (§4's "all
// must pass" rule). Calling Overall before every S1-S9 item has been added
// is meaningless by construction — OverallVerdict treats a short bundle the
// same as an empty one only when literally no items were added; a caller
// that wants "did the FULL smoke pass" must add all nine (plus any
// architecture §11 items it chooses to track) first.
func (b Bundle) Overall() Verdict {
	verdicts := make([]Verdict, len(b.Items))
	for i, item := range b.Items {
		verdicts[i] = item.Verdict
	}
	return OverallVerdict(verdicts)
}

// MissingItems reports which of the required ids have no ObserverResult yet
// — the mechanical form of "all must pass in one procedure": a bundle
// missing S6 entirely is not a bundle where S6 happened to pass, it is a
// bundle that never asked.
func (b Bundle) MissingItems(required []SmokeItemID) []SmokeItemID {
	present := make(map[SmokeItemID]bool, len(b.Items))
	for _, item := range b.Items {
		present[item.Item] = true
	}
	var missing []SmokeItemID
	for _, id := range required {
		if !present[id] {
			missing = append(missing, id)
		}
	}
	return missing
}

// RequiredSmokeItems is S1-S9 in doc order — the set goobernetes-smoke.md §4
// requires for one procedure to be complete.
func RequiredSmokeItems() []SmokeItemID {
	return []SmokeItemID{
		ItemS1FreshPod, ItemS2OSHop, ItemS3DeclaredEdge, ItemS4ArtifactAcrossNode,
		ItemS5RepassAcrossNode, ItemS6KillMatrix, ItemS7WriteAPI, ItemS8LiveVisibility,
		ItemS9NegativeControl,
	}
}

// Encode serializes the bundle as indented JSON — the durable, re-runnable
// artifact §5 rule 4 requires ("the same collateral on a rebuilt cluster
// reproduces the procedure"). Plain JSON rather than a binary or
// process-specific format so the bundle is readable without this package.
// Named Encode rather than WriteTo: `go vet`'s stdmethods check expects
// io.WriterTo's exact (int64, error) signature from any WriteTo method, and
// this bundle writer deliberately does not report a byte count.
func (b Bundle) Encode(w io.Writer) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(b)
}

// ReadBundle decodes a bundle previously written by Encode. Unknown fields
// are refused: a bundle reader silently ignoring fields it does not
// recognize is exactly the "hand edit reads as clean" failure mode D3/D2's
// falsifiability discipline exists to prevent.
func ReadBundle(r io.Reader) (Bundle, error) {
	var b Bundle
	decoder := json.NewDecoder(r)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&b); err != nil {
		return Bundle{}, fmt.Errorf("goobernetes: decode evidence bundle: %w", err)
	}
	return b, nil
}
