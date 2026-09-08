// Package prqueue defines the shared vocabulary for observed PR eligibility.
// It does not claim or mutate provider items.
package prqueue

// These codes preserve the PR selector's existing exclusion vocabulary.
const (
	Draft             = "draft"
	Checks            = "checks not passing"
	Label             = "excluded by label"
	AdvisoryPublished = "advisory verdict already published"
	Policy            = "merge-review eligibility policy"
	ScopeGate         = "scope gate"
	Escalated         = "escalated, human action required"
	Demoted           = "merge-demoted"
	SiblingBlocked    = "blocked on a sibling"
	TutorSignoff      = "awaiting human signoff"
)

// NextStep gives conservative guidance, not an instruction to remove safety
// labels or steal a claim. Unknown reasons remain explicitly unclassified.
func NextStep(reason string) string {
	switch reason {
	case Draft:
		return "Mark the pull request ready for review when its implementation is ready."
	case Checks:
		return "Wait for pending checks or remediate failing checks, then re-evaluate."
	case Label:
		return "Inspect the excluding labels and resolve their owning workflow's condition."
	case AdvisoryPublished:
		return "An advisory already covers this head; new commits require a new evaluation."
	case Policy:
		return "Inspect the workflow's opt-in, assignee, and actor-scope policy."
	case ScopeGate:
		return "Resolve the scope-gate findings and use its documented acknowledgement path."
	case Escalated:
		return "Human review and an explicit retry are required after the escalation cause is resolved."
	case Demoted:
		return "Resolve the merge-demotion cause through remediation before retrying merge review."
	case SiblingBlocked:
		return "Inspect the blocking sibling pull request and re-evaluate after it resolves."
	case TutorSignoff:
		return "Obtain the required human signoff for this tutor change."
	default:
		return "Inspect the recorded exclusion details; no automatic recovery action is known."
	}
}
