// Package childpod owns isolated generated-child execution and tree transport.
// Admission and credential authority remain in the trusted host coordinator.
package childpod

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
)

// Mandatory hard bounds cover both carrier bytes and their base64 JSON form.
const (
	MaxBundleBytes   = 16 << 20
	MaxContractBytes = 24 << 20
)

// Carrier is the bounded tree and its complete independent object custody.
type Carrier struct {
	Snapshot recovery.PortableSnapshot `json:"snapshot"`
	Bundle   []byte                    `json:"bundle"`
}

// Contract binds every pod input to its exact accepted child and attempt.
// Credentials and host filesystem paths are deliberately absent.
type Contract struct {
	ContextDigests []string                 `json:"contextDigests,omitempty"`
	KitDigest      string                   `json:"kitDigest,omitempty"`
	Version        int                      `json:"version"`
	Identity       journal.RunIdentity      `json:"identity"`
	Stage          string                   `json:"stage"`
	Attempt        int                      `json:"attempt"`
	PodAttempt     int                      `json:"podAttempt"`
	StartedAt      time.Time                `json:"startedAt"`
	Ceiling        credentials.ChildCeiling `json:"ceiling"`
	Workspace      *Carrier                 `json:"workspace,omitempty"`
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
	if c.Version != 1 || c.Identity.Child == nil || c.Identity.ValidateChildLineage() != nil || c.Stage == "" || len(c.Stage) > 256 || c.Attempt < 1 || c.PodAttempt < 1 || c.StartedAt.IsZero() {
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
