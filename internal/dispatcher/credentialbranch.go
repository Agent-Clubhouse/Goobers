package dispatcher

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/workspacerevision"
)

// ResolveBranchCheckout requests provisioning-only read access for durable ownership.
func (c *CredentialResolveClient) ResolveBranchCheckout(ctx context.Context, runID, stage string, binding *apiv1.WorkspaceBranchBinding) ([]MintedCredential, error) {
	if binding == nil {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "owned checkout requires durable binding"}
	}
	creds, err := c.resolve(ctx, runID, stage, nil, nil, binding)
	if err != nil {
		return nil, err
	}
	if creds == nil || len(creds) > 1 || len(creds) == 1 && (creds[0].Capability != WorkspaceBranchCheckoutCapability || creds[0].Value == "") {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized, Message: "owned checkout response has no exclusive checkout authorization"}
	}
	if len(creds) == 0 {
		return []MintedCredential{{Capability: WorkspaceBranchCheckoutCapability, Anonymous: true}}, nil
	}
	return creds, nil
}
