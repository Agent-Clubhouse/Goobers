package main

import (
	"context"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
)

// TestCredentialRefreshRevocationIsPerGrantNotPerAttemptNumber: a workflow
// that revisits a stage (implementation's ci-gate fail -> remediate-ci -> ...
// -> ci-poll again) restarts its attempt numbering at 1, so two executions of
// one stage in one run can carry the same run/stage/attempt. Revoking the
// first execution's grant when it returns must not refuse the second's.
func TestCredentialRefreshRevocationIsPerGrantNotPerAttemptNumber(t *testing.T) {
	service, _, runID := newRefreshFixture(t, "http://127.0.0.1:1")
	first, err := service.MintStageGrant(pushBranchEnvelope(runID, 1), []string{"repo:push"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	first.Revoke()
	second, err := service.MintStageGrant(pushBranchEnvelope(runID, 1), []string{"repo:push"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if second.Token == first.Token {
		t.Fatal("two mints of the same stage attempt produced the same grant")
	}
	if _, err := service.Refresh(context.Background(), second.Token, httpapi.CredentialRefreshRequest{Capability: "repo:push"}); err != nil {
		t.Fatalf("the revisited stage's fresh grant was refused: %v", err)
	}
	if _, err := service.Refresh(context.Background(), first.Token, httpapi.CredentialRefreshRequest{Capability: "repo:push"}); err == nil || planeErrorOf(t, err).Code != "credential_grant_revoked" {
		t.Fatalf("the finished execution's grant = %v, want credential_grant_revoked", err)
	}
}
