package workspacebranch

import (
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/workspacerevision"
)

// StageCredentialKeys is deliberately narrower than ordinary stage grants.
// Checkout credentials stay in provisioning and repo:push is local authoring
// permission only; remote publication belongs to the backend operation.
func StageCredentialKeys(keys []string, authoring bool) ([]string, error) {
	var allowed []string
	for _, key := range keys {
		switch key {
		case string(capability.AgentModel), string(capability.TelemetryRead):
			allowed = append(allowed, key)
		case string(capability.ContentsRead), string(capability.RepoRead):
			// Access to provisioned paths, not a raw repository token.
		case string(capability.RepoPush):
			if !authoring {
				return nil, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized,
					Message: "owned workspace publication requires kind=workspace-branch-publish; no raw repo:push credential is available"}
			}
		default:
			return nil, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized,
				Message: "owned workspace stages cannot materialize repository or implicit credentials: " + key}
		}
	}
	return allowed, nil
}
