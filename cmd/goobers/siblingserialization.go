package main

import (
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// Sibling-serialization strategies (#2741). Overlapping open PRs need SOME
// serialization mechanism or merge-review deadlocks: the reviewer can only
// block each PR on its siblings, so with no ordering mechanism every member of
// a cluster returns needs-changes forever and nothing lands. Until now the
// product offered exactly one such mechanism — the full election machinery
// (elect-lander/elect-gate + the deterministic overlap set gather-sibling-
// context threads as overlappingSiblings) — so a gaggle that deliberately omits
// those stages inherited the deadlock rather than a lighter default.
//
// The strategy is the SELECTION SURFACE over which cluster apply-verdict and
// elect-lander serialize; the election POLICY (fifo/newest/...) remains the
// ordering rule applied to whichever cluster the strategy yields. The two are
// orthogonal: strategy answers "who is in my cluster", policy answers "who goes
// first".
const (
	// serializationElection is the shipped default and today's exact behavior:
	// serialize only over the deterministic file-overlap set computed upstream
	// by gather-sibling-context. It is the most precise strategy — and the one
	// that requires the full election machinery to supply that set.
	serializationElection = "election"

	// serializationOrdering is the lighter default for a gaggle that omits the
	// election machinery: serialize over the reviewer-named cross-PR blockers
	// as well as any overlap set that happens to be present. It needs no extra
	// stages and no provider-label state of its own — the deterministic cluster
	// resolver (electionPolicy, fifo by default) still picks the lander, so a
	// minimal merge-review workflow gets deterministic ordering instead of a
	// permanent needs-changes loop.
	serializationOrdering = "ordering"
)

// defaultSiblingSerialization keeps the shipped behavior unchanged for every
// existing workflow: the strategy surface is additive, and opting into the
// lighter strategy is an explicit config choice.
const defaultSiblingSerialization = serializationElection

// siblingSerializationStrategies is the registry the merge-review stages
// resolve their siblingSerialization input against.
var siblingSerializationStrategies = map[string]bool{
	serializationElection: true,
	serializationOrdering: true,
}

// resolveSiblingSerialization returns the strategy actually used and whether
// the requested name was known. An unknown or empty name falls back to the
// default rather than failing the whole merge-review pipeline on a config typo
// — callers log the fallback so a misconfigured strategy is visible, not
// silent (mirroring resolveElectionPolicy).
func resolveSiblingSerialization(name string) (string, bool) {
	if siblingSerializationStrategies[name] {
		return name, true
	}
	return defaultSiblingSerialization, false
}

// serializationCluster returns the sibling PRs the selected PR must be
// serialized against under the given strategy: the deterministic overlap set
// alone under "election", and that set unioned with the reviewer's named
// cross-PR blockers under "ordering". The selected PR itself is never a member
// of its own cluster.
func serializationCluster(strategy string, findings []apiv1.Finding, overlappingSiblings []int, selectedNumber int) []int {
	seen := make(map[int]bool, len(overlappingSiblings))
	var out []int
	add := func(prs []int) {
		for _, pr := range prs {
			if pr == selectedNumber || seen[pr] {
				continue
			}
			seen[pr] = true
			out = append(out, pr)
		}
	}
	add(overlappingSiblings)
	if strategy == serializationOrdering {
		add(unionBlockingPRs(findings))
	}
	sort.Ints(out)
	return out
}
