package workspaceauthority

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// ChildWorkspaceInputName names the immutable trusted fork selector in run inputs.
const ChildWorkspaceInputName = "child-workspace"

const maxChildWorkspaceAdmissionBytes = 4096

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

// Validate checks the immutable child workspace selector.
func (a ChildWorkspaceAdmission) Validate() error {
	if !apiv1.ValidRunID(a.WorkspaceID) || len(a.WorkspaceID) > 256 || !childForkSHA.MatchString(a.ForkSHA) || !childRepositoryDigest.MatchString(a.RepositoryDigest) {
		return fmt.Errorf("runner: invalid child workspace admission")
	}
	return nil
}

// PinnedChildWorkspaceAdmission verifies the exact bounded input bytes before
// returning a recovery selector. A selector alone is not execution authority.
func PinnedChildWorkspaceAdmission(reader *journal.Reader, id journal.RunIdentity) (*ChildWorkspaceAdmission, error) {
	var ref *journal.InputRef
	for i := range id.Inputs {
		if id.Inputs[i].Name == ChildWorkspaceInputName {
			if ref != nil {
				return nil, fmt.Errorf("runner: duplicate child workspace admission")
			}
			ref = &id.Inputs[i]
		}
	}
	if ref == nil {
		return nil, nil
	}
	if id.Child == nil || ref.Integrity != apiv1.IntegrityTrusted {
		return nil, fmt.Errorf("runner: child workspace admission is not trusted child state")
	}
	data, err := reader.ArtifactBytesBounded(ref.Ref, maxChildWorkspaceAdmissionBytes)
	if err != nil {
		return nil, err
	}
	var admission ChildWorkspaceAdmission
	if err := json.Unmarshal(data, &admission); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(admission)
	if err != nil || !bytes.Equal(canonical, data) {
		return nil, fmt.Errorf("runner: child workspace admission is not canonical")
	}
	return &admission, admission.Validate()
}
