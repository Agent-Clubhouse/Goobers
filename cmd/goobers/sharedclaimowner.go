package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/sharedclaim"
)

func sharedClaimOwnerFromIdentity(instanceID string, identity journal.RunIdentity, remoteKey string) (sharedclaim.Owner, error) {
	if instanceID == "" || identity.StartedAt.IsZero() || identity.WorkflowDigest == "" || identity.Gaggle == "" || identity.Workflow == "" {
		return sharedclaim.Owner{}, fmt.Errorf("shared claim requires complete persisted instance and run ownership")
	}
	// JSON arrays avoid delimiter collisions in repository/item keys. Canonical
	// UTC timestamps make equivalent decoded run identities yield one token.
	data, err := json.Marshal([]string{instanceID, identity.RunID, identity.Gaggle, identity.Workflow, identity.WorkflowDigest, identity.StartedAt.UTC().Format(time.RFC3339Nano), remoteKey})
	if err != nil {
		return sharedclaim.Owner{}, err
	}
	owner := sharedclaim.Owner{Instance: instanceID, Run: identity.RunID, Token: fmt.Sprintf("%x", sha256.Sum256(data))}
	if _, err := sharedclaim.Encode(remoteKey, sharedclaim.Record{Version: 1, Owner: owner, ExpiresAt: identity.StartedAt}); err != nil {
		return sharedclaim.Owner{}, err
	}
	return owner, nil
}
