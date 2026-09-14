package httpapi

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/version"
)

const maxDiscoveryDocumentBytes = 4 << 20

// DiscoveryIdentity is immutable for one daemon process.
type DiscoveryIdentity struct {
	DaemonInstanceID string
	DaemonBootID     string
	Build            readservice.BuildMetadata
}

type discoveryState struct {
	identity       DiscoveryIdentity
	authentication string
	openAPI        []byte
	openAPISHA256  string
	config         handlerConfig
}

func registerDiscoveryRoutes(router *Router, config handlerConfig) (*discoveryState, error) {
	authentication := "bearer"
	switch config.authenticator.(type) {
	case NullAuthenticator, *NullAuthenticator:
		authentication = "none"
	case DenyAllAuthenticator, *DenyAllAuthenticator:
		authentication = "disabled"
	}

	identity := config.discoveryIdentity
	if identity.DaemonInstanceID == "" {
		identity.DaemonInstanceID = "unconfigured"
	}
	if identity.DaemonBootID == "" {
		var err error
		identity.DaemonBootID, err = newBootID()
		if err != nil {
			return nil, fmt.Errorf("generate daemon boot ID: %w", err)
		}
	}
	if identity.Build.Version == "" {
		build := version.Get()
		identity.Build = readservice.BuildMetadata{
			Version: build.Version,
			Commit:  build.Commit,
			Date:    build.Date,
		}
	}

	openAPI, err := apicontract.OpenAPIDocument(authentication != "none")
	if err != nil {
		return nil, err
	}
	if len(openAPI) > maxDiscoveryDocumentBytes {
		return nil, fmt.Errorf("OpenAPI document exceeds %d bytes", maxDiscoveryDocumentBytes)
	}
	state := &discoveryState{
		identity:       identity,
		authentication: authentication,
		openAPI:        openAPI,
		openAPISHA256:  sha256Hex(openAPI),
		config:         config,
	}

	router.Handle(apicontract.RouteDiscovery, state.serveDiscovery)
	router.Handle(apicontract.RouteOpenAPI, state.serveOpenAPI)
	router.Handle(apicontract.RouteCapabilities, state.serveCapabilities)
	return state, nil
}

func (s *discoveryState) serveDiscovery(w http.ResponseWriter, request *http.Request) {
	capabilityETag, err := s.capabilitiesETag()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_error", "capability document could not be encoded")
		return
	}
	summary := s.protocolSummaryWithCapabilities(capabilityETag)
	document := apicontract.DiscoveryDocument{
		ProtocolSummary: summary,
		Product:         "goobers",
		DaemonVersion:   s.identity.Build.Version,
		DaemonCommit:    s.identity.Build.Commit,
		Authentication:  s.authentication,
		APIVersions:     []string{apicontract.PreferredAPIVersion},
		Links: apicontract.DiscoveryLinks{
			Instance: apicontract.DiscoveryLink{Href: apicontract.InstancePath},
		},
		APIs: map[string]apicontract.APIDiscovery{
			apicontract.PreferredAPIVersion: {
				OpenAPI: apicontract.DiscoveryLink{
					Href:   apicontract.OpenAPIPath,
					SHA256: s.openAPISHA256,
				},
				Capabilities: apicontract.DiscoveryLink{
					Href: apicontract.CapabilitiesPath,
					ETag: capabilityETag,
				},
				Health:    apicontract.DiscoveryLink{Href: apicontract.HealthPath},
				Readiness: apicontract.DiscoveryLink{Href: apicontract.InstanceReadinessPath},
			},
		},
	}
	raw, err := marshalDocument(document)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_error", "discovery document could not be encoded")
		return
	}
	serveConditionalDocument(w, request, "application/json", "no-cache", raw)
}

func (s *discoveryState) serveOpenAPI(w http.ResponseWriter, request *http.Request) {
	serveConditionalDocument(
		w,
		request,
		"application/vnd.oai.openapi+json;version=3.1",
		"public, max-age=3600",
		s.openAPI,
	)
}

func (s *discoveryState) serveCapabilities(w http.ResponseWriter, request *http.Request) {
	raw, err := marshalDocument(s.capabilityDocument())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode_error", "capability document could not be encoded")
		return
	}
	serveConditionalDocument(w, request, "application/json", "no-cache", raw)
}

func (s *discoveryState) protocolIdentity() apicontract.ProtocolIdentity {
	return apicontract.ProtocolIdentity{
		DaemonProtocolVersion: apicontract.DaemonProtocolVersion,
		DaemonInstanceID:      s.identity.DaemonInstanceID,
		DaemonBootID:          s.identity.DaemonBootID,
		PreferredAPIVersion:   apicontract.PreferredAPIVersion,
	}
}

func (s *discoveryState) protocolSummary() (apicontract.ProtocolSummary, error) {
	capabilityETag, err := s.capabilitiesETag()
	if err != nil {
		return apicontract.ProtocolSummary{}, err
	}
	return s.protocolSummaryWithCapabilities(capabilityETag), nil
}

func (s *discoveryState) protocolSummaryWithCapabilities(capabilityETag string) apicontract.ProtocolSummary {
	return apicontract.ProtocolSummary{
		ProtocolIdentity: s.protocolIdentity(),
		OpenAPISHA256:    s.openAPISHA256,
		CapabilitiesETag: capabilityETag,
	}
}

func (s *discoveryState) capabilitiesETag() (string, error) {
	raw, err := marshalDocument(s.capabilityDocument())
	if err != nil {
		return "", err
	}
	return quotedETag(raw), nil
}

func (s *discoveryState) capabilityDocument() apicontract.CapabilityDocument {
	recovering := s.config.recoveryGate != nil && !s.config.recoveryGate()
	routes := make([]apicontract.RouteCapability, 0, len(apicontract.V1Routes()))
	for _, route := range apicontract.V1Routes() {
		available, code, reason := routeAvailability(route.ID, s.config)
		if available && recovering && !route.RecoverySafe {
			available = false
			code = "recovery_in_progress"
			reason = "daemon is completing crash recovery"
		}
		routes = append(routes, apicontract.RouteCapability{
			ID:           route.ID,
			Method:       route.Method,
			Path:         route.Path,
			ActionClass:  route.ActionClass,
			Capability:   route.Capability,
			RequiredRole: requiredRole(route),
			Remote:       apicontract.InitiallyRemoteInvocable(route.ID),
			Available:    available,
			Code:         code,
			Reason:       reason,
			Streaming:    route.Cost == apicontract.CostStream,
			RecoverySafe: route.RecoverySafe,
		})
	}
	return apicontract.CapabilityDocument{
		ProtocolIdentity: s.protocolIdentity(),
		APIVersion:       apicontract.PreferredAPIVersion,
		SchemaVersion:    1,
		OpenAPISHA256:    s.openAPISHA256,
		Routes:           routes,
	}
}

func routeAvailability(id apicontract.RouteID, config handlerConfig) (bool, string, string) {
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
		return true, "", ""
	}
	if available {
		return true, "", ""
	}
	return false, "service_unconfigured", "service is not configured on this daemon"
}

func requiredRole(route apicontract.Route) string {
	if route.Method == http.MethodGet && route.ID != apicontract.RouteRunRecovery {
		return string(RoleView)
	}
	return string(RoleOperate)
}

func marshalDocument(value any) ([]byte, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if len(raw) > maxDiscoveryDocumentBytes {
		return nil, fmt.Errorf("document exceeds %d bytes", maxDiscoveryDocumentBytes)
	}
	return raw, nil
}

func serveConditionalDocument(w http.ResponseWriter, request *http.Request, contentType, cacheControl string, raw []byte) {
	if len(raw) > maxDiscoveryDocumentBytes {
		writeError(w, http.StatusInternalServerError, "document_too_large", "API metadata document exceeds its size limit")
		return
	}
	body := raw
	etag := quotedETag(raw)
	w.Header().Set("Cache-Control", cacheControl)
	w.Header().Set("Content-Type", contentType)
	w.Header().Add("Vary", "Accept-Encoding")
	if acceptsGzip(request.Header.Get("Accept-Encoding")) {
		body = gzipDocument(raw)
		etag = "W/" + etag
		w.Header().Set("Content-Encoding", "gzip")
	}
	w.Header().Set("ETag", etag)
	if matchesIfNoneMatch(request.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

func gzipDocument(raw []byte) []byte {
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	_, _ = writer.Write(raw)
	_ = writer.Close()
	return output.Bytes()
}

func acceptsGzip(header string) bool {
	var gzipQuality, wildcardQuality *float64
	for _, value := range strings.Split(header, ",") {
		parts := strings.Split(value, ";")
		coding := strings.TrimSpace(parts[0])
		if !strings.EqualFold(coding, "gzip") && coding != "*" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			name, raw, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(name), "q") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil {
				quality = 0
			} else {
				quality = parsed
			}
		}
		if quality < 0 || quality > 1 {
			quality = 0
		}
		if strings.EqualFold(coding, "gzip") {
			gzipQuality = &quality
		} else {
			wildcardQuality = &quality
		}
	}
	if gzipQuality != nil {
		return *gzipQuality > 0
	}
	return wildcardQuality != nil && *wildcardQuality > 0
}

func quotedETag(raw []byte) string {
	return `"` + sha256Hex(raw) + `"`
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func matchesIfNoneMatch(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || normaliseETag(candidate) == normaliseETag(etag) {
			return true
		}
	}
	return false
}

func newBootID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	return fmt.Sprintf(
		"%08x-%04x-%04x-%04x-%012x",
		value[0:4],
		value[4:6],
		value[6:8],
		value[8:10],
		value[10:16],
	), nil
}

func validateDiscoveryIdentity(identity DiscoveryIdentity) error {
	if err := validateDiscoveryMetadata("daemon instance ID", identity.DaemonInstanceID, 128, true); err != nil {
		return err
	}
	if identity.DaemonBootID != "" && !validBootID(identity.DaemonBootID) {
		return errors.New("http API discovery daemon boot ID must be a UUID")
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{name: "build version", value: identity.Build.Version, limit: 128},
		{name: "build commit", value: identity.Build.Commit, limit: 128},
		{name: "build date", value: identity.Build.Date, limit: 64},
	} {
		if err := validateDiscoveryMetadata(field.name, field.value, field.limit, false); err != nil {
			return err
		}
	}
	return nil
}

func validateDiscoveryMetadata(name, value string, limit int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("http API discovery %s is required", name)
	}
	if len(value) > limit || !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("http API discovery %s must be valid bounded text without control characters", name)
	}
	return nil
}

func validBootID(value string) bool {
	if len(value) != 36 || value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
		return false
	}
	decoded := strings.ReplaceAll(value, "-", "")
	_, err := hex.DecodeString(decoded)
	return err == nil
}
