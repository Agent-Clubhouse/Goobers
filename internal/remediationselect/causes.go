package remediationselect

// Remediation policy causes (#941/PRR-6): the vocabulary a workflow's
// `remediate` input names. defaultRemediatePolicy is the shipped, fully
// liberal default — every cause pr-remediation can detect fires the agentic
// chain, reproducing today's behavior exactly.
const (
	RemediateCauseConflict       = "conflict"
	RemediateCauseSubstantive    = "substantive"
	RemediateCauseFailingCI      = "failing-ci"
	RemediateCauseBehindBase     = "behind-base"
	RemediateCauseSiblingOverlap = "sibling-overlap"
	RemediateCauseHumanComment   = "human-comment"
)
