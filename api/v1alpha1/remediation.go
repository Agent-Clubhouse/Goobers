package v1alpha1

// RemediationBriefVersion is the immutable wire identifier for the first
// remediation-brief artifact schema. Shape changes require a new version.
const RemediationBriefVersion = "goobers.dev/remediation-brief/v1"

// RemediationBrief is the evidence bundle consumed by an agentic PR-remediation
// stage. GatherPRContext is required; every other evidence section is optional
// and owned by the correspondingly named gatherer.
type RemediationBrief struct {
	Schema                 string                     `json:"schema"`
	SelectedNumber         string                     `json:"selectedNumber"`
	Head                   string                     `json:"head"`
	Base                   string                     `json:"base"`
	WorkspaceBranch        string                     `json:"workspaceBranch"`
	IsBehindBase           bool                       `json:"isBehindBase"`
	HasSubstantiveFindings string                     `json:"hasSubstantiveFindings"`
	HasFailingCI           string                     `json:"hasFailingCI"`
	GatherPRContext        RemediationPRContext       `json:"gatherPrContext"`
	GatherCIFailures       *RemediationCIFailures     `json:"gatherCIFailures,omitempty"`
	GatherReviewThreads    *RemediationReviewThreads  `json:"gatherReviewThreads,omitempty"`
	GatherSiblingContext   *RemediationSiblingContext `json:"gatherSiblingContext,omitempty"`
	GatherIssueContext     *RemediationIssueContext   `json:"gatherIssueContext,omitempty"`
}

// RemediationPRContext is the required section owned by gather-pr-context.
type RemediationPRContext struct {
	HeadSHA string `json:"headSha"`
	BaseSHA string `json:"baseSha"`
	// Verdict stays present as null when no trusted verdict exists; omission is invalid.
	Verdict  *Verdict                   `json:"verdict"`
	Comments []RemediationThreadComment `json:"comments"`
}

// RemediationThreadComment is one issue-level PR thread comment.
type RemediationThreadComment struct {
	Author    string `json:"author,omitempty"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt,omitempty"`
	URL       string `json:"url,omitempty"`
}

// RemediationCIFailures is the optional section owned by gather-ci-failures.
type RemediationCIFailures struct {
	Checks []RemediationCIFailure `json:"checks"`
}

// RemediationCIFailure describes one failing CI check and its bounded evidence.
type RemediationCIFailure struct {
	Name        string                    `json:"name"`
	Conclusion  string                    `json:"conclusion"`
	URL         string                    `json:"url,omitempty"`
	Summary     string                    `json:"summary,omitempty"`
	Annotations []RemediationCIAnnotation `json:"annotations"`
}

// RemediationCIAnnotation identifies one provider check annotation.
type RemediationCIAnnotation struct {
	Path      string `json:"path,omitempty"`
	StartLine int    `json:"startLine,omitempty"`
	EndLine   int    `json:"endLine,omitempty"`
	Level     string `json:"level,omitempty"`
	Title     string `json:"title,omitempty"`
	Message   string `json:"message"`
}

// RemediationReviewThreads is the optional section owned by
// gather-review-threads.
type RemediationReviewThreads struct {
	Reviews        []RemediationNativeReview  `json:"reviews"`
	InlineComments []RemediationInlineComment `json:"inlineComments"`
}

// RemediationNativeReview is one provider-native PR review body.
type RemediationNativeReview struct {
	Author      string `json:"author,omitempty"`
	State       string `json:"state"`
	Body        string `json:"body,omitempty"`
	CommitSHA   string `json:"commitSha,omitempty"`
	SubmittedAt string `json:"submittedAt,omitempty"`
	URL         string `json:"url,omitempty"`
}

// RemediationInlineComment is one line-level PR review comment.
type RemediationInlineComment struct {
	Author    string `json:"author,omitempty"`
	Body      string `json:"body"`
	Path      string `json:"path"`
	Line      int    `json:"line,omitempty"`
	Side      string `json:"side,omitempty"`
	InReplyTo int64  `json:"inReplyTo,omitempty"`
	CreatedAt string `json:"createdAt,omitempty"`
	URL       string `json:"url,omitempty"`
}

// RemediationSiblingContext is the optional section owned by
// gather-sibling-context.
type RemediationSiblingContext struct {
	PullRequests []RemediationSibling `json:"pullRequests"`
}

// RemediationSibling describes a related PR that can constrain remediation.
type RemediationSibling struct {
	Number           int      `json:"number"`
	Head             string   `json:"head,omitempty"`
	HeadSHA          string   `json:"headSha,omitempty"`
	Blocking         bool     `json:"blocking"`
	Reason           string   `json:"reason,omitempty"`
	OverlappingFiles []string `json:"overlappingFiles"`
}

// RemediationIssueContext is the optional section owned by
// gather-issue-context.
type RemediationIssueContext struct {
	Issues []RemediationIssue `json:"issues"`
}

// RemediationIssue is one originating issue referenced by the PR.
type RemediationIssue struct {
	Number string `json:"number"`
	Title  string `json:"title,omitempty"`
	Body   string `json:"body"`
	URL    string `json:"url,omitempty"`
}
