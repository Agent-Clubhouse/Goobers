package v1alpha1

import (
	"fmt"
	"strings"
	"unicode"
)

// WorkspaceBranchBinding is durable remote ownership, not a source ref or a
// mutable tip. StartingSHA never changes when the owned branch advances.
// +kubebuilder:object:generate=false
type WorkspaceBranchBinding struct {
	Repository  RepositoryIdentity `json:"repository"`
	Ref         string             `json:"ref"`
	StartingSHA string             `json:"startingSha"`
}

// Validate checks the closed repository identity, exact SHA, and canonical ref.
func (b WorkspaceBranchBinding) Validate() error {
	if err := b.Repository.Validate(); err != nil {
		return err
	}
	if err := ValidateCommitSHA(b.StartingSHA); err != nil {
		return err
	}
	if !strings.HasPrefix(b.Ref, "refs/heads/") || strings.HasSuffix(b.Ref, "/") ||
		strings.ContainsAny(b.Ref, " ~^:?*[\\") || strings.Contains(b.Ref, "..") ||
		strings.Contains(b.Ref, "@{") || strings.IndexFunc(b.Ref, unicode.IsControl) >= 0 {
		return fmt.Errorf("workspace branch requires a canonical refs/heads ref")
	}
	for _, component := range strings.Split(strings.TrimPrefix(b.Ref, "refs/heads/"), "/") {
		if component == "" || strings.HasPrefix(component, ".") ||
			strings.HasSuffix(component, ".") || strings.HasSuffix(component, ".lock") {
			return fmt.Errorf("workspace branch contains an invalid ref component")
		}
	}
	return nil
}

// DeepCopy returns an independent ownership value, preserving nil.
func (b *WorkspaceBranchBinding) DeepCopy() *WorkspaceBranchBinding {
	if b == nil {
		return nil
	}
	out := *b
	return &out
}
