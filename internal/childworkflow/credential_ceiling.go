package childworkflow

import (
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"

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

// Copilot may consume a GitHub PAT or stored login for model access. Existing
// declarations do not prove it cannot publish, so a no-publication child cannot
// inherit this authentication path merely under the agent:model label.
func (v *Validator) checkChildModelCredential(goober string) error {
	if v.childCredentialCeiling().AllowPublication {
		return nil
	}
	spec := v.context.Goobers[goober]
	if spec.Harness == "" || spec.Harness == apiv1.HarnessCopilot {
		return refusal("credential_isolation", "", "goober", "child model authentication may carry GitHub publication authority; a separate model-only authentication path is required")
	}
	return nil
}

func (v *Validator) checkChildGoober(gaggle, name, stage, field string) (apiv1.GooberSpec, error) {
	g, ok := v.context.Goobers[name]
	if !ok || !slices.Contains(v.context.ParentTask.ChildWorkflows.AllowedGoobers, name) || (g.Gaggle != "" && g.Gaggle != gaggle) {
		return apiv1.GooberSpec{}, refusal("goober", stage, field, "Goober is not available within the pinned child grant")
	}
	if err := v.checkChildModelCredential(name); err != nil {
		return apiv1.GooberSpec{}, err
	}
	return g, nil
}
