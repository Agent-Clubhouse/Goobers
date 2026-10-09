package main

import (
	"context"
	"net/http"

	"github.com/goobers/goobers/internal/httpapi"
)

// childCredentialPlane is installed separately from ordinary pod authority.
// It never mints a refresh grant that could outlive this physical attempt.
type childCredentialPlane struct{ service *daemonCredentialService }

func (p childCredentialPlane) Resolve(ctx context.Context, request httpapi.CredentialResolveRequest) (httpapi.CredentialResolveResponse, error) {
	refuse := func() (httpapi.CredentialResolveResponse, error) {
		return httpapi.CredentialResolveResponse{}, credentialPlaneError(http.StatusForbidden, "child_attempt_unavailable", "child credentials require the exact active execution contract")
	}
	if p.service == nil || request.Grant {
		return refuse()
	}
	attempt, err := p.service.childAttempt(ctx)
	if err != nil || request.RunID != attempt.contract.Identity.RunID || request.Stage != attempt.contract.Stage || (request.Attempt != 0 && int(request.Attempt) != attempt.contract.Attempt) || attempt.active(ctx) != nil || attempt.custody(ctx) != nil {
		return refuse()
	}
	return p.service.Resolve(ctx, request)
}
