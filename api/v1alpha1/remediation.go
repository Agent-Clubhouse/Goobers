package v1alpha1

// RemediationBriefVersion is the current remediation-brief wire identifier.
// Shape changes require a new version.
const RemediationBriefVersion = "goobers.dev/remediation-brief/v4"

// RemediationBrief is the evidence bundle consumed by an agentic PR-remediation
// stage. GatherPRContext is required; every other evidence section is optional
// and owned by the correspondingly named gatherer.
type RemediationBrief struct {
	Schema                 string                     `json:"schema"`
	Integrity              Integrity                  `json:"integrity"`
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
	// FeedbackSnapshot (v4, #6126) pins the human feedback this brief carries.
	// gather-review-threads writes it from the same provider reads it puts in
	// GatherReviewThreads and GatherPRContext.Comments; later gatherers carry
	// it forward unchanged. Absent on a brief gathered before v4.
	FeedbackSnapshot *PRFeedbackSnapshot `json:"feedbackSnapshot,omitempty"`
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
	Author    string    `json:"author,omitempty"`
	Body      string    `json:"body"`
	CreatedAt string    `json:"createdAt,omitempty"`
	URL       string    `json:"url,omitempty"`
	Integrity Integrity `json:"integrity"`
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
	Author      string    `json:"author,omitempty"`
	State       string    `json:"state"`
	Body        string    `json:"body,omitempty"`
	CommitSHA   string    `json:"commitSha,omitempty"`
	SubmittedAt string    `json:"submittedAt,omitempty"`
	URL         string    `json:"url,omitempty"`
	Integrity   Integrity `json:"integrity"`
}

// RemediationInlineComment is one line-level PR review comment.
type RemediationInlineComment struct {
	ID                int64     `json:"id,omitempty"`
	ThreadID          string    `json:"threadId,omitempty"`
	Author            string    `json:"author,omitempty"`
	Body              string    `json:"body"`
	Path              string    `json:"path"`
	Line              int       `json:"line,omitempty"`
	OriginalLine      int       `json:"originalLine,omitempty"`
	Side              string    `json:"side,omitempty"`
	StartLine         int       `json:"startLine,omitempty"`
	OriginalStartLine int       `json:"originalStartLine,omitempty"`
	StartSide         string    `json:"startSide,omitempty"`
	DiffHunk          string    `json:"diffHunk,omitempty"`
	InReplyTo         int64     `json:"inReplyTo,omitempty"`
	IsResolved        bool      `json:"isResolved"`
	IsOutdated        bool      `json:"isOutdated"`
	CreatedAt         string    `json:"createdAt,omitempty"`
	URL               string    `json:"url,omitempty"`
	Integrity         Integrity `json:"integrity"`
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
	// Ancestry is the bounded parent traversal of Issues (#6125). It is
	// present only when the stage enables parent traversal, so a brief
	// written with traversal disabled is unchanged.
	Ancestry *RemediationAncestry `json:"ancestry,omitempty"`
}

// RemediationAncestry is the bounded, provider-neutral parent ancestry of the
// originating issues. Status is "complete", "incomplete" (Omissions says what
// is missing and why) or "unsupported" (the provider has no native parent
// relation reader).
type RemediationAncestry struct {
	Status       string                        `json:"status"`
	Provider     string                        `json:"provider"`
	MaxDepth     int                           `json:"maxDepth"`
	MaxItems     int                           `json:"maxItems"`
	CrossProject string                        `json:"crossProject"`
	IncludeTypes []string                      `json:"includeTypes,omitempty"`
	Items        []RemediationAncestor         `json:"items"`
	Omissions    []RemediationAncestryOmission `json:"omissions"`
}

// RemediationAncestor is one parent work item. QualifiedID is
// provider:project:id; ParentOf lists the qualified ids of the children it
// parents; Depth is 1 for an originating issue's immediate parent.
type RemediationAncestor struct {
	QualifiedID string                     `json:"qualifiedId"`
	Provider    string                     `json:"provider"`
	Project     string                     `json:"project"`
	ID          string                     `json:"id"`
	Depth       int                        `json:"depth"`
	ParentOf    []string                   `json:"parentOf"`
	Type        string                     `json:"type,omitempty"`
	Title       string                     `json:"title,omitempty"`
	State       string                     `json:"state,omitempty"`
	URL         string                     `json:"url,omitempty"`
	Fields      []RemediationAncestorField `json:"fields"`
	Integrity   Integrity                  `json:"integrity"`
}

// RemediationAncestorField is one selected, size-bounded parent field.
type RemediationAncestorField struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	Truncated bool   `json:"truncated,omitempty"`
}

// RemediationAncestryOmission records a parent the traversal did not include.
type RemediationAncestryOmission struct {
	Child  string `json:"child"`
	Parent string `json:"parent,omitempty"`
	Depth  int    `json:"depth"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// RemediationIssue is one originating issue referenced by the PR.
type RemediationIssue struct {
	Number    string    `json:"number"`
	Title     string    `json:"title,omitempty"`
	Body      string    `json:"body"`
	URL       string    `json:"url,omitempty"`
	Integrity Integrity `json:"integrity"`
}

// remediationBriefVersions lists every wire identifier this build can read,
// newest first. Older briefs are accepted and migrated rather than rejected:
// a run that produced a v1, v2 or v3 gather artifact before this binary
// deployed must still resume into a later gatherer, which is the
// compatibility contract the retained v1/v2/v3 schemas document. A migrated
// v3 brief simply carries no FeedbackSnapshot, which every consumer treats as
// "no snapshot recorded" (#6126).
var remediationBriefVersions = []string{
	RemediationBriefVersion,
	"goobers.dev/remediation-brief/v3",
	"goobers.dev/remediation-brief/v2",
	"goobers.dev/remediation-brief/v1",
}

// SupportedRemediationBriefVersions returns the readable wire identifiers.
func SupportedRemediationBriefVersions() []string {
	out := make([]string, len(remediationBriefVersions))
	copy(out, remediationBriefVersions)
	return out
}

// SupportedRemediationBriefVersion reports whether schema is a wire identifier
// this build can read.
func SupportedRemediationBriefVersion(schema string) bool {
	for _, known := range remediationBriefVersions {
		if schema == known {
			return true
		}
	}
	return false
}

// MigrateRemediationBrief conservatively upgrades a brief decoded from an older
// wire version. Provenance introduced by v3 is absent in v1/v2, and an absent
// grade must not read as trusted — it becomes unapproved, the weakest grade, so
// a stage declaring a minimum refuses it rather than silently admitting
// pre-provenance content.
func MigrateRemediationBrief(brief RemediationBrief, schema string) RemediationBrief {
	if schema == RemediationBriefVersion {
		return brief
	}
	if brief.Integrity == "" {
		brief.Integrity = IntegrityUnapproved
	}
	brief.Schema = RemediationBriefVersion
	return brief
}
