package providers

import (
	"net/url"
	"strings"
	"time"
)

// MergeConfirmation is positive evidence returned by a landing mutation, not
// an observation that the PR was already merged. The repository address is
// derived from the provider's actual API route, never from a display gaggle,
// a bare PR number, or a caller-supplied web URL. It retains service path
// prefixes so separate installations/projects on one host cannot collide.
// Instance/run ownership comes from the enclosing immutable run journal.
type MergeConfirmation struct {
	IntentID         string `json:"intentId,omitempty"`
	RepositoryAPIURL string `json:"repositoryApiUrl"`
	PullID           string `json:"pullId"`
	MergeSHA         string `json:"mergeSha,omitempty"`
}

// QueueAdmission is a receipt for this run's accepted queue entry, not a
// completed merge. Run and instance ownership come from the enclosing journal.
// The expected head is the PR SHA pinned in the enqueue request, not a merge
// group's synthetic commit. EnqueuedAt is the forge-returned entry timestamp.
type QueueAdmission struct {
	IntentID         string    `json:"intentId,omitempty"`
	RepositoryAPIURL string    `json:"repositoryApiUrl"`
	PullID           string    `json:"pullId"`
	EntryID          string    `json:"entryId"`
	ExpectedHeadSHA  string    `json:"expectedHeadSha,omitempty"`
	EnqueuedAt       time.Time `json:"enqueuedAt"`
}

func newMergeConfirmation(repositoryAPIURL, pullID, mergeSHA string) *MergeConfirmation {
	u, err := url.Parse(repositoryAPIURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil
	}
	// Credentials and request options are never repository identity. In
	// particular, ADO's api-version query is not a different repository.
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	u.ForceQuery = false
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = strings.TrimSuffix(u.RawPath, "/")
	return &MergeConfirmation{RepositoryAPIURL: u.String(), PullID: pullID, MergeSHA: mergeSHA}
}

// MutationReceiptRunnerFields retains a durable sidecar record identity across
// local and Temporal projection. Empty identity denotes a legacy receipt; it
// must not acquire a synthetic identity when replayed.
func MutationReceiptRunnerFields(receiptID, operation string, confirmation *MergeConfirmation, admission *QueueAdmission, intent *LandingIntent) map[string]any {
	fields := MutationRunnerFields(operation, confirmation, admission, intent)
	if receiptID != "" {
		fields["mutationReceiptId"] = receiptID
	}
	return fields
}

// MutationRunnerFields preserves the legacy operation while carrying
// explicit confirmation separately. Consumers must never infer confirmation
// from operation=merge, which can also describe an observed terminal PR.
func MutationRunnerFields(operation string, confirmation *MergeConfirmation, admission *QueueAdmission, intent *LandingIntent) map[string]any {
	fields := map[string]any{"operation": operation}
	if intent != nil {
		fields["landingIntent"] = *intent
	}
	if confirmation != nil {
		fields["mergeConfirmation"] = *confirmation
	}
	if admission != nil {
		fields["queueAdmission"] = *admission
	}
	return fields
}
