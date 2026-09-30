package httpapi

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
)

// credentialrefresh.go is the credential plane's mid-stage re-resolve route
// (Goobers#6120; distributed-state-and-coordination.md DS10, §11 and
// acceptance item 8). A deterministic stage is handed, at stage start, a
// stage credential-refresh grant: a signed bearer naming its run, stage,
// attempt and exactly its declared credential capabilities, expiring with the
// stage. When a delivered value is about to expire, or the provider answered
// 401, the stage presents the grant here and receives a fresh value for ONE
// capability the grant names.
//
// WHO MAY CALL: the holder of a valid grant, and nobody else. A human or
// operator principal, a pod token and a worker token are all refused, so no
// existing principal gains anything from this route. It is served on a local
// daemon's loopback API too (local stages have no other daemon channel), where
// the request is otherwise anonymous: the grant is the whole authorization,
// verified by the service, never by the request's absence of a principal.
//
// Agentic stages receive no grant in phase 1; the service also refuses to
// refresh for one. TODO(Goobers#6120 phase 2): agentic harness tokens.

// CredentialGrantTokenPrefix restates podauth.CredentialGrantPrefix: podauth
// imports this package, so importing it back would be a cycle. Pinned by
// TestCredentialGrantPrefixMatchesPodauth.
const CredentialGrantTokenPrefix = "goobers-grant."

// CredentialRefreshRequest names the one capability to re-resolve. The run,
// stage and attempt come from the grant, never from the body.
type CredentialRefreshRequest struct {
	Capability string `json:"capability"`
}

// CredentialRefreshService re-resolves one capability for the stage a grant
// names. Implementations verify the grant (signature, expiry), confine the
// capability to the grant's list, re-verify the stage against the run's
// pinned definition, refuse agentic stages, rate-limit per grant, journal
// each re-resolve, and register the value with the scrubbers before
// returning. Errors carry their HTTP status as an *InterventionError.
type CredentialRefreshService interface {
	Refresh(ctx context.Context, grant string, request CredentialRefreshRequest) (CredentialResolveResponse, error)
}

func registerCredentialRefreshRoute(router *Router, credentials CredentialService, errorLog *log.Logger) {
	router.Handle(apicontract.RouteCredentialRefresh, func(w http.ResponseWriter, request *http.Request) {
		refresher, ok := credentials.(CredentialRefreshService)
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "credentials_unavailable", "the credential refresh route is not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		grant, ok := credentialGrantFromRequest(request)
		if !ok {
			writeError(w, http.StatusForbidden, "credential_refresh_requires_grant",
				"the credential refresh route accepts only a stage credential-refresh grant")
			return
		}
		var input CredentialRefreshRequest
		if err := decodeWriteRequest(request, &input); err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
			return
		}
		if strings.TrimSpace(input.Capability) == "" || len(input.Capability) > MaxCredentialCapabilityBytes {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest,
				fmt.Sprintf("capability must be non-empty and no longer than %d bytes", MaxCredentialCapabilityBytes))
			return
		}
		response, err := refresher.Refresh(request.Context(), grant, input)
		if err != nil {
			writePlaneError(w, errorLog, "refresh credential", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, response)
	})
}

// credentialGrantFromRequest returns the grant bearer, refusing a request
// that an authenticator established as any OTHER principal.
func credentialGrantFromRequest(request *http.Request) (string, bool) {
	if principal, authenticated := PrincipalFromRequest(request); authenticated && principal.Issuer != CredentialGrantPrincipalIssuer {
		return "", false
	}
	authorization := request.Header.Get("Authorization")
	const scheme = "Bearer "
	if len(authorization) <= len(scheme) || !strings.EqualFold(authorization[:len(scheme)], scheme) {
		return "", false
	}
	token := strings.TrimSpace(authorization[len(scheme):])
	if !strings.HasPrefix(token, CredentialGrantTokenPrefix) {
		return "", false
	}
	return token, true
}
