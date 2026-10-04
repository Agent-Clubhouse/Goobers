// Package childpod owns isolated generated-child execution and tree transport.
// Admission and credential authority remain in the trusted host coordinator.
package childpod

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

// Mandatory hard bounds cover both carrier bytes and their base64 JSON form.
const (
	MaxBundleBytes   = 16 << 20
	MaxContractBytes = 24 << 20
	Command          = "__dispatch-child-exec"
)

// WorkspaceInput is trusted host custody, never taken from task inputs.
type WorkspaceInput struct {
	Path string
	Fork recovery.ChildSnapshot
}

// Request is assembled from the accepted source, committed stage attempt and
// pinned run identity. No mutable-name lookup belongs in this adapter.
type Request struct {
	ParentOrigin *apiv1.ChildWorkflowOrigin
	Identity     journal.RunIdentity
	Attempt      dispatcher.Attempt
	Eligible     []dispatcher.RunnerSpec
	Workspace    *WorkspaceInput
	Ceiling      credentials.ChildCeiling
	StartedAt    time.Time
}

// Carrier is the bounded tree and its complete independent object custody.
type Carrier struct {
	Snapshot recovery.PortableSnapshot `json:"snapshot"`
	Bundle   []byte                    `json:"bundle"`
}

// Contract binds every pod input to its exact accepted child and attempt.
// Credentials and host filesystem paths are deliberately absent.
type Contract struct {
	ContextDigests []string                   `json:"contextDigests,omitempty"`
	ParentOrigin   *apiv1.ChildWorkflowOrigin `json:"parentOrigin,omitempty"`
	KitDigest      string                     `json:"kitDigest,omitempty"`
	Version        int                        `json:"version"`
	Identity       journal.RunIdentity        `json:"identity"`
	Stage          string                     `json:"stage"`
	Attempt        int                        `json:"attempt"`
	PodAttempt     int                        `json:"podAttempt"`
	StartedAt      time.Time                  `json:"startedAt"`
	Ceiling        credentials.ChildCeiling   `json:"ceiling"`
	Workspace      *Carrier                   `json:"workspace,omitempty"`
}

// Output returns a tree bound to its input contract. Pod commit history is
// deliberately squashed when mapped back onto the host's real child fork.
type Output struct {
	Version        int      `json:"version"`
	ContractDigest string   `json:"contractDigest"`
	Workspace      *Carrier `json:"workspace,omitempty"`
}

// Validate checks immutable identity and supported credential delegation.
func (c Contract) Validate() error {
	if len(c.ContextDigests) > 64 || !slices.IsSorted(c.ContextDigests) {
		return fmt.Errorf("invalid isolated context digests")
	}
	for i, digest := range c.ContextDigests {
		if !blobstore.ValidDigest(digest) || (i > 0 && c.ContextDigests[i-1] == digest) {
			return fmt.Errorf("invalid isolated context digest")
		}
	}
	if c.KitDigest != "" && !blobstore.ValidDigest(c.KitDigest) {
		return fmt.Errorf("invalid isolated child kit digest")
	}
	if c.Version != 1 || c.validateOwner() != nil || c.Stage == "" || len(c.Stage) > 256 || c.Attempt < 1 || c.PodAttempt < 1 || c.StartedAt.IsZero() {
		return fmt.Errorf("invalid isolated child contract identity")
	}
	if err := c.Ceiling.Validate(); err != nil {
		return err
	}
	if c.Ceiling.AllowPublication {
		return fmt.Errorf("isolated tree transport does not support child PR publication")
	}
	if c.Workspace != nil {
		return c.Workspace.Validate()
	}
	return nil
}

// Validate verifies metadata and exact bundle bytes before any Git import.
func (c Carrier) Validate() error {
	if err := c.Snapshot.Validate(); err != nil {
		return err
	}
	if len(c.Bundle) == 0 || len(c.Bundle) > MaxBundleBytes || int64(len(c.Bundle)) != c.Snapshot.Record.ArchiveBytes || journal.Digest(c.Bundle) != c.Snapshot.Record.ArchiveDigest {
		return fmt.Errorf("invalid isolated child workspace carrier")
	}
	return nil
}

// DecodeContract refuses noncanonical or oversized protocol documents.
func DecodeContract(data []byte, digest string) (Contract, error) {
	var c Contract
	if err := decode(data, digest, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

// DecodeOutput checks the source binding, policy, repository and exact bundle.
func DecodeOutput(data []byte, digest, contractDigest string, c Contract) (Output, error) {
	var out Output
	if err := decode(data, digest, &out); err != nil {
		return out, err
	}
	if out.Version != 1 || out.ContractDigest != contractDigest || (out.Workspace == nil) != (c.Workspace == nil) {
		return out, fmt.Errorf("child output contract mismatch")
	}
	if out.Workspace != nil {
		if err := out.Workspace.Validate(); err != nil {
			return out, err
		}
		if out.Workspace.Snapshot.Record.RepositoryKey != c.Workspace.Snapshot.Record.RepositoryKey || !reflect.DeepEqual(out.Workspace.Snapshot.Policy, c.Workspace.Snapshot.Policy) {
			return out, fmt.Errorf("child output workspace policy mismatch")
		}
	}
	return out, nil
}

func decode(data []byte, digest string, out any) error {
	if len(data) == 0 || len(data) > MaxContractBytes || journal.Digest(data) != digest {
		return fmt.Errorf("isolated child document digest or size mismatch")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	canonical, err := json.Marshal(out)
	if err != nil || !bytes.Equal(canonical, data) {
		return fmt.Errorf("noncanonical isolated child document")
	}
	return nil
}

func (c Contract) validateOwner() error {
	if c.ParentOrigin == nil {
		if c.Identity.Child == nil {
			return fmt.Errorf("isolated contract has no owner")
		}
		return c.Identity.ValidateChildLineage()
	}
	if c.Identity.Child != nil || !apiv1.ValidRunID(c.Identity.RunID) || c.Identity.InstanceID == "" || c.Identity.Gaggle == "" || c.Identity.Workflow == "" {
		return fmt.Errorf("invalid isolated parent owner")
	}
	for _, digest := range []string{c.Identity.ConfigGeneration, c.Identity.WorkflowDigest, c.Identity.GooberDigest} {
		if !blobstore.ValidDigest(digest) {
			return fmt.Errorf("invalid isolated parent source")
		}
	}
	if c.ParentOrigin.AttemptID == "" || c.ParentOrigin.StageOccurrence == "" || len(c.ParentOrigin.AttemptID) > 256 || len(c.ParentOrigin.StageOccurrence) > 256 {
		return fmt.Errorf("invalid isolated parent occurrence")
	}
	return nil
}
