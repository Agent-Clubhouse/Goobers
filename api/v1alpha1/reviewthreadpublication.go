package v1alpha1

// ReviewThreadPublicationVersion is the review-thread publication receipt wire
// identifier (#6131). Shape changes require a new version.
const ReviewThreadPublicationVersion = "goobers.dev/review-thread-publication/v1"

// Review-thread publication statuses.
const (
	// ReviewThreadPublicationInProgress is written before the first mutation
	// and after each verified one. A receipt left in this state belongs to an
	// interrupted attempt.
	ReviewThreadPublicationInProgress = "in_progress"
	// ReviewThreadPublicationComplete means every intended mutation was
	// verified by reading it back.
	ReviewThreadPublicationComplete = "complete"
	// ReviewThreadPublicationPartial means an error stopped publication after
	// at least one mutation was verified.
	ReviewThreadPublicationPartial = "partial"
	// ReviewThreadPublicationFailed means an error stopped publication before
	// any mutation was verified.
	ReviewThreadPublicationFailed = "failed"
	// ReviewThreadPublicationStale means publication stopped because the PR's
	// head or feedback no longer matched what this run answered. Threads
	// records exactly which mutations had completed.
	ReviewThreadPublicationStale = "stale"
)

// Per-mutation states in a ReviewThreadReceipt.
const (
	ReviewThreadMutationPending       = "pending"
	ReviewThreadMutationVerified      = "verified"
	ReviewThreadMutationFailed        = "failed"
	ReviewThreadMutationNotApplicable = "not_applicable"
)

// How a retry accounted for a mutation it did not make itself.
const (
	// ReviewThreadRecoveryReceiptConfirmed: an earlier attempt's receipt
	// recorded the mutation and provider state still shows it.
	ReviewThreadRecoveryReceiptConfirmed = "receipt_confirmed"
	// ReviewThreadRecoveryProviderAdopted: provider state shows the mutation
	// (this run's reply marker, or the resolution) but no receipt recorded it,
	// as after an attempt interrupted between mutation and receipt write.
	ReviewThreadRecoveryProviderAdopted = "provider_adopted"
	// ReviewThreadRecoveryEarlierPass: an earlier publication pass of this run
	// (before a feedback repass) already answered the same thread content
	// with the same disposition at the same head, and provider state still
	// shows that reply, so this pass reuses it instead of repeating it.
	ReviewThreadRecoveryEarlierPass = "earlier_pass"
)

// ReviewThreadRestorationUnsupported records that no provider-side rollback
// is attempted: already published, human-visible replies are never deleted on
// a generic failure, so a partial publication is preserved and reported.
const ReviewThreadRestorationUnsupported = "unsupported"

// PRFeedbackStaleReason is one structured reason live pull-request state
// differs from the feedback snapshot a run answered (#6126).
type PRFeedbackStaleReason struct {
	Code   string `json:"code"`
	Kind   string `json:"kind"`
	ID     string `json:"id,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ReviewThreadPublication is resolve-review-threads' durable, versioned
// transaction record. It is the stage's result file, rewritten after every
// verified mutation, so a stage that stops part-way leaves exact evidence of
// what completed and the executor journals it whatever the exit. A retry
// reconciles against the newest matching receipt in its own run journal
// instead of rediscovering its work from provider state alone. Provider state
// stays authoritative: a retry re-reads every thread before trusting an entry.
//
// SelectedNumber, PublishedHeadSHA, UnresolvedThreadCount and StaleInput are
// also the stage's scalar outputs, so their JSON names match the workflow's
// existing output contract.
type ReviewThreadPublication struct {
	Schema                 string                  `json:"schema"`
	Integrity              Integrity               `json:"integrity"`
	PullRequest            string                  `json:"pullRequest"`
	SelectedNumber         string                  `json:"selectedNumber"`
	PublishedHeadSHA       string                  `json:"publishedHeadSha"`
	FeedbackSnapshotDigest string                  `json:"feedbackSnapshotDigest"`
	Status                 string                  `json:"status"`
	ResumedFromReceipt     bool                    `json:"resumedFromReceipt"`
	Restoration            string                  `json:"restoration"`
	UnresolvedThreadCount  string                  `json:"unresolvedThreadCount,omitempty"`
	StaleInput             string                  `json:"staleInput"`
	StaleReasons           []PRFeedbackStaleReason `json:"staleReasons,omitempty"`
	Threads                []ReviewThreadReceipt   `json:"threads"`

	// NoWork ends the run: the PR head moved off the published SHA.
	NoWork       bool   `json:"noWork,omitempty"`
	NoWorkReason string `json:"noWorkReason,omitempty"`
	Outcome      string `json:"outcome,omitempty"`
	LiveHeadSHA  string `json:"liveHeadSha,omitempty"`

	// Typed stage failure, set only when the stage exits non-zero.
	ErrorCode      string `json:"errorCode,omitempty"`
	ErrorMessage   string `json:"errorMessage,omitempty"`
	ErrorRetryable bool   `json:"errorRetryable,omitempty"`
	RateLimitReset string `json:"rateLimitReset,omitempty"`
}

// ReviewThreadReceipt is one thread's transaction history.
type ReviewThreadReceipt struct {
	ThreadID        string `json:"threadId"`
	Disposition     string `json:"disposition"`
	ContentDigest   string `json:"contentDigest,omitempty"`
	ReplyState      string `json:"replyState"`
	ResolutionState string `json:"resolutionState"`
	ProviderReplyID string `json:"providerReplyId,omitempty"`
	Recovery        string `json:"recovery,omitempty"`
	LastError       string `json:"lastError,omitempty"`
}
