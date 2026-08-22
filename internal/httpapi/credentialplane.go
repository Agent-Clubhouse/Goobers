package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

// credentialplane.go implements the daemon write API's credential plane
// (distributed-state-and-coordination.md §11, DS9/DS10): the resolve endpoint
// a stage pod calls at stage start to receive short-lived credentials scoped
// to exactly its stage's declared credential capabilities. Dispatch payloads
// carry opaque references only (#2931) — this route is where the reference
// becomes a value, daemon-side, through the same capability-gated machinery
// the local runner's buildCredentialEnv resolves through.
//
// CONSUMER CONTRACT (DS10, the #3489 lesson):
//   - Resolve at STAGE START. Never inherit credential values from dispatch
//     time or pod-creation time — a dispatch-time snapshot's remaining life is
//     unknowable, and a 22-minute stage has already outlived one in production.
//   - Honor ExpiresAt when present. A response value is a snapshot of a
//     refreshing daemon-side source, not a lease the pod can assume outlives
//     the stage.
//   - One re-resolve-and-retry on 401. When a provider call fails with an
//     auth error mid-stage, the consumer re-issues the SAME resolve request
//     (fresh values are returned; the plane holds no cache), retries the
//     provider call once, and only then fails the attempt. This is what keeps
//     stage timeoutSeconds from being silently bounded by token life.
//
// The plane holds no resolved values beyond the request lifetime, and every
// value it returns is registered with the daemon's journal scrubbers before
// the response is written, so a later leak into any journal or log line is
// redacted.

// MaxCredentialResolveCapabilities bounds the optional capability-subset
// filter. A stage declares a handful of capabilities; a request naming more
// than this is malformed, not ambitious.
const MaxCredentialResolveCapabilities = 32

// MaxCredentialCapabilityBytes bounds one capability name in the request.
const MaxCredentialCapabilityBytes = 128

// CredentialResolveRequest asks the credential plane for the calling stage's
// credentials. RunID and Stage identify which stage of which run is asking;
// the service verifies Stage against the run's pinned workflow definition, so
// a pod cannot resolve another stage's broader grants. Capabilities optionally
// narrows resolution to a subset of the stage's declared credential
// capabilities (the re-resolve path typically names just the capability that
// 401'd); empty resolves the full declared set. Naming a capability the stage
// did not declare is refused with a typed 403 naming the capability — nothing
// materializes for an undeclared capability.
type CredentialResolveRequest struct {
	RunID        string   `json:"runId"`
	Stage        string   `json:"stage"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// MintedCredential is one resolved credential value. ExpiresAt is present
// when the backing source states an expiry (GitHub App installation tokens
// do); absent means the source stated none — the consumer must still treat
// the value as a snapshot, not a lease (DS10).
type MintedCredential struct {
	Capability string     `json:"capability"`
	Value      string     `json:"value"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
}

// CredentialResolveResponse carries the minted credentials for one resolve
// call. Capabilities that are declared but carry no configured grant are
// simply absent (not every capability is credentialed), mirroring
// buildCredentialEnv's behavior on the worker path.
type CredentialResolveResponse struct {
	RunID       string             `json:"runId"`
	Stage       string             `json:"stage"`
	Credentials []MintedCredential `json:"credentials"`
}

// CredentialService is the daemon-side credential plane. Implementations
// verify the stage identity against the run's pinned definition, resolve
// through the capability-gated injector machinery (fail closed: nothing for
// an undeclared capability), register every resolved value with the journal
// scrubbers BEFORE returning, journal an audit event naming which capabilities
// were resolved for which stage (never values), and hold no resolved values
// beyond the call.
type CredentialService interface {
	Resolve(ctx context.Context, request CredentialResolveRequest) (CredentialResolveResponse, error)
}

// WithCredentialService enables the credential-plane resolve route.
func WithCredentialService(credentials CredentialService) HandlerOption {
	return func(config *handlerConfig) error {
		if credentials == nil {
			return errors.New("http API credential service is required")
		}
		config.credentials = credentials
		return nil
	}
}

func registerCredentialRoute(router *Router, credentials CredentialService, errorLog *log.Logger) {
	router.Handle(apicontract.RouteCredentialResolve, func(w http.ResponseWriter, request *http.Request) {
		if credentials == nil {
			writeError(w, http.StatusServiceUnavailable, "credentials_unavailable", "the credential plane is not available from this server")
			return
		}
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		var input CredentialResolveRequest
		if err := decodeWriteRequest(request, &input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if strings.TrimSpace(input.RunID) == "" || strings.TrimSpace(input.Stage) == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "runId and stage are required")
			return
		}
		if len(input.Capabilities) > MaxCredentialResolveCapabilities {
			writeError(w, http.StatusBadRequest, "invalid_request",
				fmt.Sprintf("capabilities must name no more than %d entries", MaxCredentialResolveCapabilities))
			return
		}
		for _, capability := range input.Capabilities {
			if strings.TrimSpace(capability) == "" || len(capability) > MaxCredentialCapabilityBytes {
				writeError(w, http.StatusBadRequest, "invalid_request",
					fmt.Sprintf("capability names must be non-empty and no longer than %d bytes", MaxCredentialCapabilityBytes))
				return
			}
		}
		// DS9 posture: this plane serves stage pods. A pod token proves "I am
		// run X's stage pod" and authorizes credential resolution for run X's
		// own stages and no other run's. An authenticated HUMAN principal is
		// refused outright — the plane is a secret-disclosure surface, and no
		// human workflow needs it (operators hold instance access already; the
		// smoke drives it through real pod tokens). An unauthenticated request
		// only exists on the loopback null-auth posture, where the caller is
		// local-trusted and could read the instance config directly.
		if principal, ok := PrincipalFromRequest(request); ok {
			if !IsPodPrincipal(principal) {
				writeError(w, http.StatusForbidden, "pod_principal_required", "the credential plane serves stage pods only")
				return
			}
			if principal.Subject != podPrincipalSubject(input.RunID) {
				writeError(w, http.StatusForbidden, "run_mismatch", "pod principal may only resolve credentials for its own run")
				return
			}
		}
		response, err := credentials.Resolve(request.Context(), input)
		if err != nil {
			writePlaneError(w, errorLog, "resolve credentials", err)
			return
		}
		// The body carries live secret material: forbid every cache layer.
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, response)
	})
}
