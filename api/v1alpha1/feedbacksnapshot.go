package v1alpha1

// PRFeedbackSnapshotVersion is the pull-request feedback snapshot wire
// identifier (#6126). Shape changes require a new version.
const PRFeedbackSnapshotVersion = "goobers.dev/pr-feedback-snapshot/v1"

// PRFeedbackSnapshot is the immutable identity of the human feedback a
// remediation run answered: the exact pull-request head plus every relevant
// general comment, native review body and review thread, canonicalized so
// equivalent provider payloads produce the same SnapshotDigest.
//
// It records identities and content digests, not bodies: the bodies the agent
// read live in the enclosing remediation brief. Provider payload mechanics
// (URLs, anchors, retrieval order, display formatting) are deliberately
// absent, so the digest changes only when the feedback does.
type PRFeedbackSnapshot struct {
	Schema      string `json:"schema"`
	Provider    string `json:"provider"`
	Repository  string `json:"repository"`
	PullRequest string `json:"pullRequest"`
	HeadSHA     string `json:"headSHA"`
	// CapturedAt is informational and excluded from SnapshotDigest.
	CapturedAt string `json:"capturedAt"`
	// Complete is true only when every source was read in full; it is part of
	// the digest input so an incomplete read can never collide with a clean one.
	Complete        bool                `json:"complete"`
	GeneralComments []PRFeedbackComment `json:"generalComments"`
	Reviews         []PRFeedbackReview  `json:"reviews"`
	ReviewThreads   []PRFeedbackThread  `json:"reviewThreads"`
	SnapshotDigest  string              `json:"snapshotDigest"`
}

// PRFeedbackComment is one human comment's identity and content digest.
type PRFeedbackComment struct {
	ID         string `json:"id"`
	Author     string `json:"author"`
	InReplyTo  string `json:"inReplyTo,omitempty"`
	BodySHA256 string `json:"bodySha256"`
}

// PRFeedbackReview is one native review body's identity and content digest.
// Review state is deliberately absent: providers rewrite it on push
// (stale-review dismissal) without any human acting.
type PRFeedbackReview struct {
	ID         string `json:"id"`
	Author     string `json:"author"`
	BodySHA256 string `json:"bodySha256"`
}

// PRFeedbackThread is one review thread: its resolution state, and its human
// comments. ContentDigest covers the comments only (not resolution or
// outdated state), so it names "what the reviewers said on this thread".
type PRFeedbackThread struct {
	ThreadID      string              `json:"threadId"`
	Resolved      bool                `json:"resolved"`
	Outdated      bool                `json:"outdated"`
	ContentDigest string              `json:"contentDigest"`
	Comments      []PRFeedbackComment `json:"comments"`
}
