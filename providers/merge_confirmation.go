package providers

import (
	"net/url"
	"strings"
)

// MergeConfirmation is positive evidence returned by a landing mutation, not
// an observation that the PR was already merged. The repository address is
// derived from the provider's actual API route, never from a display gaggle,
// a bare PR number, or a caller-supplied web URL. It retains service path
// prefixes so separate installations/projects on one host cannot collide.
// Instance/run ownership comes from the enclosing immutable run journal.
type MergeConfirmation struct {
	RepositoryAPIURL string `json:"repositoryApiUrl"`
	PullID           string `json:"pullId"`
	MergeSHA         string `json:"mergeSha,omitempty"`
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

// MutationRunnerFields preserves the legacy operation while carrying
// explicit confirmation separately. Consumers must never infer confirmation
// from operation=merge, which can also describe an observed terminal PR.
func MutationRunnerFields(operation string, confirmation *MergeConfirmation) map[string]any {
	fields := map[string]any{"operation": operation}
	if confirmation != nil {
		fields["mergeConfirmation"] = *confirmation
	}
	return fields
}
