package childworkflow

import (
	"slices"

	"github.com/goobers/goobers/internal/credentials"
)

// CredentialCeiling returns independent host-owned delegation derived during
// trusted proposal validation. Editing exported proposal fields cannot widen it.
func (p *Proposal) CredentialCeiling() credentials.ChildCeiling {
	out := p.credentialCeiling
	out.AllowedKeys = slices.Clone(out.AllowedKeys)
	return out
}

func (v *Validator) childCredentialCeiling() credentials.ChildCeiling {
	policy := v.context.ParentTask.ChildWorkflows
	return credentials.NewChildCeiling(policy.AllowPRPublication && v.context.AllowPRPublication, v.context.GrantedCapabilities, policy.AllowedCapabilities)
}
