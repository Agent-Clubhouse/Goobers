package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/livejournal"
)

var errControllerJournalAuthority = errors.New("controller journal authority refused")

// WithControllerJournalVerifier enables exact-batch controller authentication.
// Ordinary journal principals never gain lifecycle-origin authority.
func WithControllerJournalVerifier(verifier livejournal.ControllerJournalVerifier) HandlerOption {
	return func(c *handlerConfig) error {
		if verifier == nil {
			return errControllerJournalAuthority
		}
		c.controllerJournal = verifier
		return nil
	}
}

func emitJournalWithAuthority(request *http.Request, input livejournal.EmitRequest, service JournalService, verifier livejournal.ControllerJournalVerifier) (livejournal.EmitResponse, error) {
	token, _ := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	principal, authenticated := PrincipalFromRequest(request)
	if !strings.HasPrefix(token, livejournal.ControllerJournalTokenPrefix) && principal.Issuer != ControllerJournalPrincipalIssuer {
		return service.Emit(request.Context(), input)
	}
	if verifier == nil || (authenticated && principal.Issuer != ControllerJournalPrincipalIssuer) {
		return livejournal.EmitResponse{}, errControllerJournalAuthority
	}
	runID, digest, err := verifier.VerifyControllerJournal(token)
	if err != nil || runID != input.RunID {
		return livejournal.EmitResponse{}, errControllerJournalAuthority
	}
	actual, err := livejournal.ControllerJournalDigest(input)
	if err != nil || actual != digest {
		return livejournal.EmitResponse{}, errControllerJournalAuthority
	}
	controller, ok := service.(interface {
		EmitController(context.Context, livejournal.EmitRequest) (livejournal.EmitResponse, error)
	})
	if !ok {
		return livejournal.EmitResponse{}, errControllerJournalAuthority
	}
	return controller.EmitController(request.Context(), input)
}

func authorizeControllerJournal(request *http.Request) error {
	if request.Method == http.MethodPost && journalPlanePath(request.URL.Path) {
		return nil
	}
	return errors.New("controller journal capability is confined to journal emission")
}
