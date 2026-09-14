package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
)

func registerDiscoveryRoutes(router *Router, reader readservice.Reader, errorLog *log.Logger, config handlerConfig) error {
	authentication := "bearer"
	switch config.authenticator.(type) {
	case NullAuthenticator, *NullAuthenticator:
		authentication = "none"
	case DenyAllAuthenticator, *DenyAllAuthenticator:
		authentication = "disabled"
	}
	openAPI, err := apicontract.OpenAPIDocument(authentication == "bearer")
	if err != nil {
		return err
	}
	sum := sha256.Sum256(openAPI)
	digest := hex.EncodeToString(sum[:])

	router.Handle(apicontract.RouteDiscovery, func(w http.ResponseWriter, request *http.Request) {
		health, err := reader.Health(request.Context())
		if err != nil {
			errorLog.Printf("API discovery health read failed: %v", err)
			writeError(w, http.StatusInternalServerError, "read_error", "daemon identity could not be read")
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, http.StatusOK, apicontract.DiscoveryDocument{
			Product:             "goobers",
			DaemonVersion:       health.Build.Version,
			DaemonCommit:        health.Build.Commit,
			Authentication:      authentication,
			PreferredAPIVersion: "v1",
			APIVersions:         []string{"v1"},
			OpenAPI:             apicontract.OpenAPIPath,
			Capabilities:        apicontract.CapabilitiesPath,
			Instance:            apicontract.InstancePath,
			Health:              apicontract.HealthPath,
			OpenAPISHA256:       digest,
		})
	})
	router.Handle(apicontract.RouteOpenAPI, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Header().Set("Content-Type", "application/vnd.oai.openapi+json;version=3.1")
		w.Header().Set("ETag", `"`+digest+`"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(openAPI)
	})
	router.Handle(apicontract.RouteCapabilities, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		writeJSON(w, http.StatusOK, capabilityDocument(config, digest))
	})
	return nil
}

func capabilityDocument(config handlerConfig, digest string) apicontract.CapabilityDocument {
	recovering := config.recoveryGate != nil && !config.recoveryGate()
	routes := make([]apicontract.RouteCapability, 0, len(apicontract.V1Routes()))
	for _, route := range apicontract.V1Routes() {
		available, reason := routeAvailability(route.ID, config)
		if available && recovering && !route.RecoverySafe {
			available = false
			reason = "daemon is completing crash recovery"
		}
		routes = append(routes, apicontract.RouteCapability{
			ID:           route.ID,
			Method:       route.Method,
			Path:         route.Path,
			ActionClass:  route.ActionClass,
			Capability:   route.Capability,
			RequiredRole: requiredRole(route),
			Available:    available,
			Reason:       reason,
			Streaming:    route.Cost == apicontract.CostStream,
			RecoverySafe: route.RecoverySafe,
		})
	}
	return apicontract.CapabilityDocument{
		APIVersion:    "v1",
		SchemaVersion: 1,
		OpenAPISHA256: digest,
		Routes:        routes,
	}
}

func routeAvailability(id apicontract.RouteID, config handlerConfig) (bool, string) {
	var available bool
	switch id {
	case apicontract.RouteInstanceReadiness:
		available = config.instanceReadiness != nil
	case apicontract.RouteConfigDigest:
		available = config.configDigest != nil
	case apicontract.RouteWorkerConfigDivergence:
		available = config.workerConfigDivergence != nil
	case apicontract.RouteEvents:
		available = config.events != nil
	case apicontract.RouteRunReveal:
		available = config.runRevealer != nil
	case apicontract.RouteApproveStage, apicontract.RouteOverrideStage, apicontract.RouteRerunStage:
		available = config.interventions != nil
	case apicontract.RouteWorkflowEnabled:
		available = config.workflowMutations != nil
	case apicontract.RouteClaimAcquire, apicontract.RouteClaimRenew, apicontract.RouteClaimRelease,
		apicontract.RouteClaimSettle, apicontract.RouteClaimList, apicontract.RouteClaimVerify,
		apicontract.RouteClaimRecover:
		available = config.claims != nil
	case apicontract.RouteTriggerIngest, apicontract.RouteTriggerStatus:
		available = config.triggers != nil
	case apicontract.RouteResolveEscalation:
		available = config.escalations != nil
	case apicontract.RouteCancelRun:
		available = config.cancels != nil
	case apicontract.RouteJournalEmit:
		available = config.journal != nil
	case apicontract.RouteJournalRunPhase, apicontract.RouteJournalConflictTouches,
		apicontract.RouteJournalUnpushedWork, apicontract.RouteJournalEscalationCandidates,
		apicontract.RouteJournalBranchOwnership:
		available = config.runJournal != nil
	case apicontract.RouteCredentialResolve:
		available = config.credentials != nil
	case apicontract.RouteBlobGet, apicontract.RouteBlobPut:
		available = config.blobs != nil
	case apicontract.RouteRunRecovery, apicontract.RouteRunRecoveryPublish:
		available = config.recovery != nil
	case apicontract.RouteStageSurrender:
		available = config.surrenders != nil
	case apicontract.RouteGaggleStateGet, apicontract.RouteGaggleStatePut:
		available = config.state != nil
	case apicontract.RouteTelemetryDefectAggregates:
		available = config.telemetryDefects != nil
	default:
		return true, ""
	}
	if available {
		return true, ""
	}
	return false, "service is not configured on this daemon"
}

func requiredRole(route apicontract.Route) string {
	if route.Method == http.MethodGet && route.ID != apicontract.RouteRunRecovery {
		return string(RoleView)
	}
	return string(RoleOperate)
}
