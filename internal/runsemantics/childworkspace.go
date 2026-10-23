package runsemantics

import (
	"fmt"
	"regexp"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// ChildWorkspaceAdmission is supplied by the trusted child launcher after it
// provisions an isolated managed fork. It carries no path, URL or credentials.
// The journal pins it as an immutable trusted input; resume never substitutes
// current configuration or creates a replacement for missing custody.
type ChildWorkspaceAdmission struct {
	WorkspaceID      string `json:"workspaceId"`
	ForkSHA          string `json:"forkSha"`
	RepositoryDigest string `json:"repositoryDigest"`
}

var childForkSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
var childRepositoryDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate checks the immutable workspace identity and digests.
func (a ChildWorkspaceAdmission) Validate() error {
	if !apiv1.ValidRunID(a.WorkspaceID) || len(a.WorkspaceID) > 256 || !childForkSHA.MatchString(a.ForkSHA) || !childRepositoryDigest.MatchString(a.RepositoryDigest) {
		return fmt.Errorf("runner: invalid child workspace admission")
	}
	return nil
}
