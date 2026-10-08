// Package lifecycle defines Goobers lifecycle labels with code-owned semantics.
package lifecycle

// Goobers lifecycle labels with code-defined producer/consumer semantics.
const (
	LabelApproved         = "goobers:approved"
	LabelClaimed          = "goobers:claimed"
	LabelReady            = "goobers:ready"
	LabelNeedsHuman       = "goobers:needs-human"
	LabelNeedsRemediation = "goobers:needs-remediation"
	LabelBlockedOnSibling = "goobers:blocked-on-sibling"
	LabelMergeEscalated   = "goobers:merge-escalated"
	LabelStatusInReview   = "goobers/status:in-review"
)
