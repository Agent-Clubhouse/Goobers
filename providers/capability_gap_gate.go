package providers

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// This file implements #3058's second half: a forge-aware gate over the
// capability-gap registry. ValidateGapRegistry (capability_matrix.go)
// checks each entry's shape offline; the check here needs a forge, because
// only the forge knows whether a tracked gap's issue is still open. A
// tracked entry pointing at a closed issue is exactly the rot this gate
// exists to catch: the gap is either fixed (drop the entry), still real
// under a different issue (repoint it), or permanent (record it as
// GapNotApplicable instead).

// trackedGapIssuePattern is the reference form the registry stores:
// "#NNN", the bare cross-reference forges auto-link within a repository.
var trackedGapIssuePattern = regexp.MustCompile(`^#[1-9][0-9]*$`)

// closedIssueStates are the provider-native states that mean "this issue
// is no longer open" across the forges the registry can be checked
// against (GitHub's "closed", ADO work-item "Completed"/"Resolved"/
// "Removed", Gitea's "closed").
var closedIssueStates = []string{"closed", "completed", "resolved", "done", "removed", "abandoned"}

// TrackedGapReference is one GapTracked registry entry, flattened for
// checking against a forge.
type TrackedGapReference struct {
	Provider   ProviderKind
	Capability Capability
	Issue      string
}

// TrackedGapReferences returns every tracked (fixable) gap in the
// registry, in a deterministic provider-then-capability order. Permanent
// GapNotApplicable entries are excluded: they name no issue, so there is
// nothing for a forge to verify.
func TrackedGapReferences() []TrackedGapReference {
	var refs []TrackedGapReference
	for _, kind := range AllProviderKinds() {
		caps := knownGaps[kind]
		for _, cap := range AllCapabilities() {
			gap, ok := caps[cap]
			if !ok || gap.Kind != GapTracked {
				continue
			}
			refs = append(refs, TrackedGapReference{Provider: kind, Capability: cap, Issue: gap.Issue})
		}
	}
	return slices.Clip(refs)
}

// GapIssueLookup reads a single backlog item from the forge that hosts the
// registry's issue references. Every BacklogProvider satisfies it, so the
// gate runs against whichever forge the repository actually lives on
// rather than assuming GitHub.
type GapIssueLookup interface {
	GetWorkItem(context.Context, RepositoryRef, string) (WorkItem, error)
}

// ValidateTrackedGapsOpen is the forge-aware gate: every GapTracked entry
// must reference an issue that is still open on repo's forge. Returns one
// actionable error per violation (including lookups that failed, since an
// unverifiable reference is not a verified one); a clean run returns nil.
func ValidateTrackedGapsOpen(ctx context.Context, lookup GapIssueLookup, repo RepositoryRef) []error {
	if lookup == nil {
		return []error{fmt.Errorf("tracked-gap gate needs a backlog lookup to resolve issue references on %s", repoLabel(repo))}
	}
	var errs []error
	for _, ref := range TrackedGapReferences() {
		if !trackedGapIssuePattern.MatchString(ref.Issue) {
			errs = append(errs, fmt.Errorf(
				"knownGaps[%q][%q] issue reference %q is not of the form \"#NNN\" and cannot be resolved on %s",
				ref.Provider, ref.Capability, ref.Issue, repoLabel(repo)))
			continue
		}
		id := strings.TrimPrefix(ref.Issue, "#")
		item, err := lookup.GetWorkItem(ctx, repo, id)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"knownGaps[%q][%q] issue %s could not be resolved on %s: %w",
				ref.Provider, ref.Capability, ref.Issue, repoLabel(repo), err))
			continue
		}
		if issueStateIsClosed(item.State) {
			errs = append(errs, fmt.Errorf(
				"knownGaps[%q][%q] tracks issue %s, which is %s — repoint it at the issue tracking the remaining work, drop the entry if the gap is fixed, or record it as a permanent %s gap with a rationale",
				ref.Provider, ref.Capability, ref.Issue, item.State, GapNotApplicable))
		}
	}
	return errs
}

// issueStateIsClosed reports whether a provider-native issue state means
// the issue is no longer open. An unrecognized (or empty) state is treated
// as open so a forge whose vocabulary this does not model yet cannot
// manufacture a false failure.
func issueStateIsClosed(state string) bool {
	return slices.ContainsFunc(closedIssueStates, func(closed string) bool {
		return strings.EqualFold(state, closed)
	})
}

// repoLabel renders a repository reference for an error message, keeping
// the ADO project segment when there is one.
func repoLabel(repo RepositoryRef) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{repo.Owner, repo.Project, repo.Name} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "/")
}
