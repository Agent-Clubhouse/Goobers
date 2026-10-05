package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/sessioning"
)

// PRRepairRecoveryService authorizes current operators and original source custody.
type PRRepairRecoveryService interface {
	Get(context.Context, Principal, string, string) (sessioning.PRRepairCommandView, error)
	Check(context.Context, Principal, string, string) (sessioning.PRRepairCommandView, error)
}

// WithPRRepairRecovery installs post-turn observation, never mutation replay.
func WithPRRepairRecovery(service PRRepairRecoveryService) HandlerOption {
	return func(c *handlerConfig) error {
		if service == nil {
			return errors.New("repair recovery service is required")
		}
		c.prRepairRecovery = service
		return nil
	}
}
func registerPRRepairRecovery(router *Router, c handlerConfig, errorLog *log.Logger) {
	router.Handle(apicontract.RoutePRRepairCommand, prRepairRecoveryHandler(c.prRepairRecovery, false, errorLog))
	router.Handle(apicontract.RoutePRRepairCheck, prRepairRecoveryHandler(c.prRepairRecovery, true, errorLog))
}
func prRepairRecoveryHandler(service PRRepairRecoveryService, check bool, errorLog *log.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		p, ok := interactiveHuman(w, r)
		if !ok {
			return
		}
		if !p.HasRole(RoleOperate) || p.ChildWorkflow != nil || p.GeneratedChild || p.WorkflowParent || len(p.Scopes) != 0 {
			writeError(w, 403, "interactive_access_denied", "Repair observation requires a human operator.")
			return
		}
		id := r.PathValue("command")
		if r.URL.RawQuery != "" || r.URL.ForceQuery || (!check && (r.ContentLength != 0 || len(r.TransferEncoding) > 0)) || len(id) != 39 || !strings.HasPrefix(id, "repair-") || strings.Trim(id[7:], "0123456789abcdef") != "" {
			writeError(w, 400, CodeInvalidRequest, "Choose an exact retained repair command without target overrides.")
			return
		}
		if check && !emptyRepairCheck(r) {
			writeError(w, 400, CodeInvalidRequest, "Use one empty JSON object for an explicit repair check.")
			return
		}
		if service == nil {
			writeError(w, 503, "pr_repair_recovery_unavailable", "Repair observation is unavailable.")
			return
		}
		var value sessioning.PRRepairCommandView
		var err error
		if check {
			value, err = service.Check(r.Context(), p, r.PathValue("gaggle"), id)
		} else {
			value, err = service.Get(r.Context(), p, r.PathValue("gaggle"), id)
		}
		if err != nil {
			writePlaneError(w, errorLog, "PR repair observation", err)
			return
		}
		raw, err := json.Marshal(value)
		if err != nil || len(raw)+1 > 128<<10 {
			writeError(w, 502, "pr_repair_response_invalid", "Repair evidence exceeded its bounded contract.")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(append(raw, '\n'))
	}
}
func emptyRepairCheck(r *http.Request) bool {
	if validateInteractiveTransport(r) != nil {
		return false
	}
	defer func() { _ = r.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1025))
	if err != nil || len(raw) > 1024 {
		return false
	}
	_, ok := metadataJSONObject(raw, []string{}, []string{})
	return ok
}
