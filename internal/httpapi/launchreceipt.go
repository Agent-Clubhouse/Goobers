package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/launchreceipt"
)

// LaunchReceiptService verifies authority and persists one immutable receipt.
type LaunchReceiptService interface {
	Accept(context.Context, string, launchreceipt.Receipt) error
}

func authorizeLaunchReceipt(request *http.Request, principal Principal) (bool, error) {
	if request.URL.Path != apicontract.LaunchReceiptPath && principal.Issuer != LaunchGrantPrincipalIssuer {
		return false, nil
	}
	if request.Method == http.MethodPost && request.URL.Path == apicontract.LaunchReceiptPath && principal.Issuer == LaunchGrantPrincipalIssuer {
		return true, nil
	}
	return true, errors.New("launch grant is confined to launch receipt persistence")
}

// WithLaunchReceiptService wires the private, write-only controller plane.
func WithLaunchReceiptService(service LaunchReceiptService) HandlerOption {
	return func(c *handlerConfig) error {
		if service == nil {
			return errors.New("launch receipt service required")
		}
		c.launchReceipts = service
		return nil
	}
}

func registerLaunchReceiptRoute(router *Router, service LaunchReceiptService) {
	router.Handle(apicontract.RouteLaunchReceipt, func(w http.ResponseWriter, request *http.Request) {
		if status, code, message := validateMutationTransport(request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		if principal, ok := PrincipalFromRequest(request); ok && principal.Issuer != LaunchGrantPrincipalIssuer {
			writeError(w, http.StatusForbidden, "launch_authority_required", "only a launch grant may persist a receipt")
			return
		}
		token, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
		if !ok || !strings.HasPrefix(token, launchreceipt.TokenPrefix) || len(token) > 1024 {
			writeError(w, http.StatusForbidden, "launch_authority_required", "a launch grant is required")
			return
		}
		if service == nil {
			writeError(w, http.StatusServiceUnavailable, "launch_receipts_unavailable", "launch receipt service unavailable")
			return
		}
		defer func() { _ = request.Body.Close() }()
		body := http.MaxBytesReader(w, request.Body, launchreceipt.MaxBytes)
		decoder := json.NewDecoder(body)
		decoder.DisallowUnknownFields()
		var receipt launchreceipt.Receipt
		if decoder.Decode(&receipt) != nil || decoder.Decode(new(any)) != io.EOF {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid launch receipt")
			return
		}
		if err := service.Accept(request.Context(), token, receipt); err != nil {
			status := http.StatusServiceUnavailable
			if errors.Is(err, launchreceipt.ErrInvalid) {
				status = http.StatusForbidden
			}
			if errors.Is(err, launchreceipt.ErrUsed) {
				status = http.StatusConflict
			}
			// Never echo credentials, decoded configuration, or filesystem errors.
			writeError(w, status, "launch_receipt_refused", "launch receipt was not accepted")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
