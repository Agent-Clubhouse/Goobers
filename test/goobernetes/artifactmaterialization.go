package goobernetes

import (
	"fmt"

	"github.com/goobers/goobers/internal/readservice"
)

// ArtifactMaterializationObserver is S4's named observer
// (goobernetes-smoke.md §4 S4): "the artifact.recorded journal event
// (digest, producing attempt) on node A's attempt, and node B's attempt
// journal showing the same digest materialized before stage start."
const ArtifactMaterializationObserver = "StageAttempt.Artifacts (internal/readservice/runs.go, artifact.recorded on the producing attempt) + the consuming attempt's materialize-before-start record"

// ArtifactConsumption is the consumer-side half of S4's observer: the
// digest a later stage declared as input and whether it was materialized
// BEFORE that stage started (materialize-before-stage, per goobernetes-
// smoke.md S4). There is no read-model field for this today —
// StageAttempt.Artifacts (internal/readservice/runs.go:449) records only
// artifacts a given attempt PRODUCED (artifact.recorded matched to the
// producing attempt, internal/readservice/runs.go:2688-2696), and
// cross-node write-through-before-dispose materialization is mode-3 runtime
// behavior the #3513 dispatcher does not exist to exercise yet
// (runnersolve.go's ExecutableSubstrate doc comment names the same gap).
// A live driver supplies this from whatever the eventual materialize path
// records (a new journal field, or the executor's own integrity-check log);
// this struct is the seam.
type ArtifactConsumption struct {
	ConsumerStage string
	// MaterializedDigest is the digest the consumer actually read at input
	// time, "" if materialization never happened or failed.
	MaterializedDigest string
	// MaterializedBeforeStart reports whether the digest was available
	// before the consuming stage's stage.started event (materialize-before-
	// stage, not lazily fetched mid-run).
	MaterializedBeforeStart bool
	// IntegrityCheckFailed reports whether the executor's integrity check
	// classified a missing/mismatched blob (#2866) rather than crashing
	// hard. Only meaningful when MaterializedDigest == "".
	IntegrityCheckFailed bool
}

// AssertArtifactMaterialization is S4: a stage on node A records an
// artifact; a later stage on node B declares it as input and receives it,
// materialized before that stage starts.
//
// producer is node A's AttemptList for the producing stage (the same shape
// AssertFreshPodPerAttempt consumes); wantDigest is the artifact digest the
// consumer is expected to have received; consumption is the consumer-side
// observation (see ArtifactConsumption's doc comment for why this is
// caller-supplied rather than read off a fixed field today).
//
// Per S4's explicit rule, a missing blob must fail SOFT (classified by
// #2866's integrity check) — IntegrityCheckFailed=true with an empty
// MaterializedDigest is reported as a genuine FAIL (the artifact truly did
// not cross nodes), never conflated with a hard crash, which S4 calls out as
// "a fail of the classification contract even if retry recovers."
func AssertArtifactMaterialization(producer readservice.AttemptList, wantDigest string, consumption ArtifactConsumption) AssertionResult {
	if wantDigest == "" {
		return invalid("no target artifact digest named", nil)
	}
	if len(producer.Attempts) == 0 {
		return invalid(fmt.Sprintf("producer stage %q has no attempts", producer.Stage), nil)
	}

	var recorded *readservice.ArtifactMetadata
	for _, a := range producer.Attempts {
		for i := range a.Artifacts {
			if a.Artifacts[i].Digest == wantDigest {
				recorded = &a.Artifacts[i]
			}
		}
	}
	if recorded == nil {
		return invalid(fmt.Sprintf("digest %q was never recorded by producer stage %q — the artifact.recorded observer never fired for it", wantDigest, producer.Stage), nil)
	}

	if consumption.ConsumerStage == "" {
		return invalid("no consumer stage named in the consumption evidence", nil)
	}
	if consumption.MaterializedDigest == "" {
		detail := fmt.Sprintf("consumer stage %q never materialized digest %q", consumption.ConsumerStage, wantDigest)
		if consumption.IntegrityCheckFailed {
			// Soft-failed and classified: still a FAIL of S4 (the artifact
			// did not cross nodes), but the classification contract itself
			// held — recorded distinctly in Evidence so a bundle reader can
			// tell the two failure shapes apart.
			return classify("", false, detail+" (soft-failed: classified by the executor's integrity check, #2866 — the classification contract held)", nil, consumption)
		}
		return classify("", false, detail+" (no integrity-check classification recorded — if this was a missing blob, a hard crash occurred instead of a soft classified failure, itself a fail per S4)", nil, consumption)
	}
	if consumption.MaterializedDigest != wantDigest {
		return classify("", false, fmt.Sprintf("consumer stage %q materialized digest %q, want %q", consumption.ConsumerStage, consumption.MaterializedDigest, wantDigest), nil, consumption)
	}
	if !consumption.MaterializedBeforeStart {
		return classify("", false, fmt.Sprintf("consumer stage %q received digest %q but not before stage start (materialize-before-stage violated)", consumption.ConsumerStage, wantDigest), nil, consumption)
	}
	return classify("", true, "", map[string]any{"producer": *recorded, "consumer": consumption}, nil)
}
