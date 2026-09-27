package httpapi

import (
	"context"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

// InstanceReadinessService backs the readiness-gate endpoint (#5019). It is
// the one service the recovery gate in Router.serve never blocks, so it must
// never depend on readservice.Reader or anything else that is unsafe to
// query before crash-orphan recovery completes.
//
// Named distinctly from ReadinessCheck/ReadinessStatus (probes.go's
// pre-router, unauthenticated /readyz): this is the richer, authenticated
// sibling of RouteInstance, not a replacement for /readyz.
type InstanceReadinessService interface {
	InstanceReadiness(ctx context.Context) (InstanceReadiness, error)
}

// InstanceReadiness is RouteInstanceReadiness's response. It deliberately
// carries only durable identity plus recovery/readiness state — never the
// inventory RouteInstance exposes — so that route's response contract stays
// unchanged (#5019) while this one stays servable during recovery.
type InstanceReadiness struct {
	APIVersion    string                       `json:"apiVersion"`
	SchemaVersion string                       `json:"schemaVersion"`
	Protocol      *apicontract.ProtocolSummary `json:"protocol,omitempty"`
	ComputerName  string                       `json:"computerName,omitempty"`
	InstanceRoot  string                       `json:"instanceRoot"`
	RootIdentity  *readservice.RootIdentity    `json:"rootIdentity,omitempty"`
	// Ready is the same overall gate /readyz and /api/v1/health.Ready read,
	// so no surface can ever disagree about whether recovery has completed.
	Ready    bool                  `json:"ready"`
	Recovery InstanceRecoveryPhase `json:"recovery"`
}

// InstanceRecoveryPhase names the startup phase runUpContext is currently
// executing (or the last one it completed, once Ready is true) and how long
// it has been in that phase, plus the measured worktree/recovery accumulation
// and the duration budget derived from it. BudgetState becomes "approaching"
// at 80% and "exceeded" at 100%, giving operators the same signal the startup
// watchdog uses instead of a separate hand-tuned threshold.
type InstanceRecoveryPhase struct {
	Phase             string  `json:"phase"`
	Target            string  `json:"target,omitempty"`
	ElapsedSeconds    float64 `json:"elapsedSeconds"`
	WorktreeCount     int     `json:"worktreeCount"`
	RecoveryRunCount  int     `json:"recoveryRunCount"`
	AccumulationCount int     `json:"accumulationCount"`
	BudgetSeconds     float64 `json:"budgetSeconds"`
	BudgetUsedPercent float64 `json:"budgetUsedPercent"`
	BudgetState       string  `json:"budgetState"`
}

func registerInstanceReadinessRoute(router *Router, svc InstanceReadinessService, errorLog *log.Logger, discovery *discoveryState) {
	router.Handle(apicontract.RouteInstanceReadiness, func(w http.ResponseWriter, request *http.Request) {
		if svc == nil {
			writeError(w, http.StatusServiceUnavailable, "instance_readiness_unavailable", "this daemon does not publish readiness state")
			return
		}
		value, err := svc.InstanceReadiness(request.Context())
		if err != nil {
			errorLog.Printf("instance readiness read failed: %v", err)
			writeError(w, http.StatusInternalServerError, "read_error", "readiness state could not be read")
			return
		}
		value.Protocol, err = discovery.protocolSummary()
		if err != nil {
			errorLog.Printf("instance readiness protocol summary failed: %v", err)
			writeError(w, http.StatusInternalServerError, "encode_error", "protocol summary could not be encoded")
			return
		}
		writeJSON(w, http.StatusOK, value)
	})
}
