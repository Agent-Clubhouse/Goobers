package providers

import (
	"fmt"
	"slices"
)

// This file implements CONF-2 (#2075, design doc §5/§7 item 2): the
// generated capability x provider matrix and the blessed-tier CI gate.
// docs/provider-capability-matrix.md (cmd/goobers/capabilitymatrixdoc.go)
// renders BuildMatrix()'s output; TestBlessedTierGapsAreTracked enforces
// ValidateBlessedTier() in CI.

// AllCapabilities returns the full closed capability enum (§3.1), in the
// grouped order capability.go documents them. New capabilities are added
// here at the same time as their constant — the matrix must never silently
// drop a row.
func AllCapabilities() []Capability {
	return []Capability{
		CapRepoClone, CapRepoBranch, CapRepoCommit, CapRepoPush,
		CapPROpen, CapPRList, CapPRPoll, CapPRClose, CapPRFiles, CapPRCompare,
		CapPRQueryAuthor, CapPRQueryAssignee, CapPRQueryRequestedReviewer,
		CapPRReviewRequest, CapPRReviewSubmit, CapPRReviewThreads, CapPRReviewResolve,
		CapPRMerge, CapPRLandingDetectPolicy, CapPRLandingEnqueue, CapPRLandingPoll, CapPRUpdateBranch, CapBranchDelete,
		CapRepoPolicyRead, CapPRStatusPublish,
		CapBacklogList, CapBacklogGet, CapBacklogComments, CapBacklogCreate, CapBacklogUpdate, CapBacklogStatus, CapBacklogClaim, CapBacklogBlockers,
		CapTriggerSubscribe,
	}
}

// ProviderSupportLevel describes the maturity of a registered provider.
type ProviderSupportLevel string

const (
	// ProviderSupported identifies providers backed by the repository's support evidence.
	ProviderSupported ProviderSupportLevel = "supported"
	// ProviderExperimental identifies providers that have not met their promotion criteria.
	ProviderExperimental ProviderSupportLevel = "experimental"
)

// ProviderSupport describes one provider's product support designation.
type ProviderSupport struct {
	Provider          ProviderKind
	Level             ProviderSupportLevel
	PromotionCriteria string
}

var providerSupport = []ProviderSupport{
	{Provider: ProviderGitHub, Level: ProviderSupported},
	{Provider: ProviderADO, Level: ProviderSupported},
	{
		Provider:          ProviderGitea,
		Level:             ProviderExperimental,
		PromotionCriteria: "an in-repo `provider: gitea` config exercised in merge-tier CI and live conformance per #2441",
	},
}

// AllProviderSupport returns every provider's support designation, blessed
// tier first.
func AllProviderSupport() []ProviderSupport {
	return slices.Clone(providerSupport)
}

// AllProviderKinds returns every provider kind the matrix covers, blessed
// tier first.
func AllProviderKinds() []ProviderKind {
	support := AllProviderSupport()
	kinds := make([]ProviderKind, 0, len(support))
	for _, provider := range support {
		kinds = append(kinds, provider.Provider)
	}
	return kinds
}

// BlessedTierProviderKinds are the providers the design doc's §2 goal holds
// to full conformance on the workflow-required set: "GitHub and ADO
// together... move in lockstep from here on."
func BlessedTierProviderKinds() []ProviderKind {
	return []ProviderKind{ProviderGitHub, ProviderADO}
}

// WorkflowRequiredCapabilities is the interim stand-in for CONF-6's
// (#2079) requires.capabilities registry, which does not exist yet: the
// capability subset the epic's V1 acceptance gate (design doc §8) actually
// needs for an ADO repo to complete issue -> curated -> implemented ->
// merged autonomously. The blessed-tier rule (§5) is enforced against this
// set, not the full enum — capabilities outside it (e.g. repo-policy
// reads) are informational in the matrix and do not gate CI, because
// nothing in the shipped workflows reaches them through a provider-neutral
// path today (they are still hardcoded to
// *providers.GitHubProvider at their call sites). Once CONF-6 lands a real
// per-workflow registry, this can be replaced by deriving the union of
// every shipped workflow's requirements instead of hand-maintaining it.
func WorkflowRequiredCapabilities() CapabilitySet {
	return mandatoryCapabilities().With(
		CapPRMerge, CapPRLandingDetectPolicy, CapPRLandingEnqueue, CapPRLandingPoll,
		CapBranchDelete, CapPRCompare,
		CapPRQueryAuthor, CapPRQueryAssignee, CapPRQueryRequestedReviewer,
		CapBacklogBlockers,
	)
}

// CapabilityGapKind distinguishes the two things a registry entry can
// mean. Collapsing them onto one "linked issue" string made a permanent
// forge difference indistinguishable from unfinished work, and forced
// non-applicable cells to point at an issue that will never be open
// (#3058).
type CapabilityGapKind string

const (
	// GapTracked is a fixable divergence: the provider should reach
	// conformance, and the named issue tracks getting there.
	GapTracked CapabilityGapKind = "tracked"
	// GapNotApplicable is a permanent difference in the forge itself:
	// there is nothing to fix, so the entry carries a rationale instead of
	// an issue reference.
	GapNotApplicable CapabilityGapKind = "not-applicable"
)

// CapabilityGap is one registry entry. A GapTracked entry carries Issue
// (and no Rationale); a GapNotApplicable entry carries Rationale (and no
// Issue). ValidateGapRegistry enforces that split.
type CapabilityGap struct {
	Kind      CapabilityGapKind
	Issue     string // e.g. "#3030"; GapTracked only.
	Rationale string // why the capability can never apply; GapNotApplicable only.
}

// knownGaps is CONF-2's gap registry (design doc §5: "gap (linked issue) /
// not-applicable"). Every blessed-tier (provider, capability) pair inside
// WorkflowRequiredCapabilities() that is not fully conformant today MUST
// appear here — either as a GapTracked entry naming the issue tracking the
// fix, or as a GapNotApplicable entry explaining why the capability cannot
// exist on that forge — or TestBlessedTierGapsAreTracked fails CI with an
// actionable message. Remove a GapTracked entry only once its provider's
// declaration and behavior have actually caught up (CONF-3/#2076 removed
// the landing-group entries once it implemented them and flipped ADO's
// declaration).
var knownGaps = map[ProviderKind]map[Capability]CapabilityGap{
	ProviderADO: {
		CapBacklogBlockers: {Kind: GapTracked, Issue: "#3030"},
		CapPRQueryAssignee: {
			Kind:      GapNotApplicable,
			Rationale: "Azure DevOps pull requests have no assignee concept; reviewers are the closest analog and are covered by pr.query.requestedReviewer",
		},
	},
}

// gapFor returns the registry entry for (kind, cap), and whether one is
// registered.
func gapFor(kind ProviderKind, cap Capability) (CapabilityGap, bool) {
	gap, ok := knownGaps[kind][cap]
	return gap, ok
}

// MatrixStatus is a matrix cell's rendered state.
type MatrixStatus string

const (
	// StatusConformant is declared with no tracked defect.
	StatusConformant MatrixStatus = "conformant"
	// StatusDeclaredGap is declared, but a tracked issue says the behavior
	// behind it is not actually conformant (e.g. backlog.blockers/#2059's
	// fail-open stub, which compiles and is declared but lies).
	StatusDeclaredGap MatrixStatus = "declared (gap)"
	// StatusGap is not declared, with a tracked issue explaining why.
	StatusGap MatrixStatus = "gap"
	// StatusNotApplicable is not declared because the capability cannot
	// exist on that forge (design doc §5's "not-applicable" state). Unlike
	// StatusGap it is permanent: no issue tracks it, because there is
	// nothing to fix.
	StatusNotApplicable MatrixStatus = "not applicable"
	// StatusNotDeclared is not declared with no tracked issue — legal for
	// a capability outside WorkflowRequiredCapabilities(), or for a
	// community adapter, which owes no gap-issue justification (§5:
	// "conformant on whatever they declare — nothing more, but nothing
	// less").
	StatusNotDeclared MatrixStatus = "not declared"
)

// MatrixCell is one (capability, provider) row x column entry.
type MatrixCell struct {
	Capability Capability
	Provider   ProviderKind
	Status     MatrixStatus
	GapKind    CapabilityGapKind // empty unless a registry entry covers this cell.
	GapIssue   string            // e.g. "#2076"; empty unless Status is a tracked gap state.
	Rationale  string            // why the capability is permanently not applicable; StatusNotApplicable only.
}

func classify(kind ProviderKind, cap Capability, declared bool) MatrixCell {
	gap, registered := gapFor(kind, cap)
	cell := MatrixCell{Capability: cap, Provider: kind}
	if registered {
		cell.GapKind = gap.Kind
		cell.GapIssue = gap.Issue
		cell.Rationale = gap.Rationale
	}
	switch {
	case registered && gap.Kind == GapNotApplicable:
		cell.Status = StatusNotApplicable
	case declared && registered:
		cell.Status = StatusDeclaredGap
	case declared:
		cell.Status = StatusConformant
	case registered:
		cell.Status = StatusGap
	default:
		cell.Status = StatusNotDeclared
	}
	return cell
}

// BuildMatrix computes the full capability x provider matrix from live
// declarations and the knownGaps registry — the single source of truth
// both the generated doc and the blessed-tier CI gate read from, so
// neither can drift from the other or from what providers actually
// declare.
func BuildMatrix() []MatrixCell {
	var cells []MatrixCell
	for _, cap := range AllCapabilities() {
		for _, kind := range AllProviderKinds() {
			declared, _ := CapabilitiesFor(kind)
			cells = append(cells, classify(kind, cap, declared.Has(cap)))
		}
	}
	return cells
}

// ValidateBlessedTier enforces the design doc §5 blessed-tier rule: every
// capability in WorkflowRequiredCapabilities() must be conformant or
// covered by a registry entry (StatusDeclaredGap/StatusGap for a tracked
// divergence, StatusNotApplicable for a permanent one) for every
// blessed-tier provider — StatusNotDeclared inside the required set is the
// one forbidden state, an undocumented divergence. A capability a provider
// actually declares must not be registered as permanently not applicable
// either: that contradiction would render a working capability as absent.
// Returns one actionable error per violation; a clean run returns nil.
func ValidateBlessedTier() []error {
	var errs []error
	required := WorkflowRequiredCapabilities()
	for _, kind := range BlessedTierProviderKinds() {
		declared, _ := CapabilitiesFor(kind)
		for cap := range required {
			isDeclared := declared.Has(cap)
			cell := classify(kind, cap, isDeclared)
			if cell.Status == StatusNotDeclared {
				errs = append(errs, fmt.Errorf(
					"blessed-tier provider %q does not declare required capability %q and has no gap entry in providers/capability_matrix.go's knownGaps registry",
					kind, cap))
			}
			if isDeclared && cell.GapKind == GapNotApplicable {
				errs = append(errs, fmt.Errorf(
					"provider %q declares capability %q, but knownGaps records it as permanently not applicable — drop the registry entry or stop declaring the capability",
					kind, cap))
			}
		}
	}
	return errs
}

// ValidateGapRegistry checks each knownGaps entry is well formed for its
// kind: a tracked gap names an issue and no rationale, a not-applicable
// gap carries a rationale and no issue reference. This keeps the two
// representations distinguishable rather than letting a permanent
// difference quietly borrow an issue link (#3058).
func ValidateGapRegistry() []error {
	var errs []error
	for _, kind := range AllProviderKinds() {
		caps := knownGaps[kind]
		for _, cap := range AllCapabilities() {
			gap, ok := caps[cap]
			if !ok {
				continue
			}
			switch gap.Kind {
			case GapTracked:
				if !trackedGapIssuePattern.MatchString(gap.Issue) {
					errs = append(errs, fmt.Errorf(
						"knownGaps[%q][%q] is a tracked gap but its issue reference %q is not of the form \"#NNN\"", kind, cap, gap.Issue))
				}
				if gap.Rationale != "" {
					errs = append(errs, fmt.Errorf(
						"knownGaps[%q][%q] is a tracked gap but carries a not-applicable rationale — tracked gaps are explained by their issue", kind, cap))
				}
			case GapNotApplicable:
				if gap.Rationale == "" {
					errs = append(errs, fmt.Errorf(
						"knownGaps[%q][%q] is not applicable but carries no rationale explaining why the capability cannot exist on that forge", kind, cap))
				}
				if gap.Issue != "" {
					errs = append(errs, fmt.Errorf(
						"knownGaps[%q][%q] is not applicable but references issue %q — a permanent difference has no fix to track", kind, cap, gap.Issue))
				}
			default:
				errs = append(errs, fmt.Errorf(
					"knownGaps[%q][%q] has unknown gap kind %q, want %q or %q", kind, cap, gap.Kind, GapTracked, GapNotApplicable))
			}
		}
	}
	return errs
}
