package temporalcodec

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"

	commonpb "go.temporal.io/api/common/v1"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/oidcauth"
)

// MaxHTTPBodyBytes bounds the remote contract's base64-encoded JSON body.
const MaxHTTPBodyBytes = 16 << 20

type httpHandler struct {
	codec   *Codec
	auth    httpapi.Authenticator
	origins map[string]bool
	slots   chan struct{}
}

// NewHTTPHandler serves Temporal's POST /encode and /decode contract with
// mandatory OIDC authentication and an explicit view-role check on both routes.
// It never chooses local/anonymous authentication. CORS origins are exact
// scheme/host/port values; an empty list disables browser cross-origin access.
// The caller owns listener TLS, HTTP server timeouts, and lifecycle.
func NewHTTPHandler(codec *Codec, authConfig oidcauth.Config, allowedOrigins []string) (http.Handler, error) {
	if codec == nil {
		return nil, fmt.Errorf("codec HTTP handler requires a codec")
	}
	auth, err := oidcauth.New(authConfig)
	if err != nil {
		return nil, err
	}
	origins := make(map[string]bool, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || origin != parsed.Scheme+"://"+parsed.Host {
			return nil, fmt.Errorf("codec HTTP origin must be an explicit http(s) origin")
		}
		origins[origin] = true
	}
	return &httpHandler{codec: codec, auth: auth, origins: origins, slots: make(chan struct{}, 4)}, nil
}

func (h *httpHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path != "/encode" && r.URL.Path != "/decode" {
		http.NotFound(w, r)
		return
	}
	if !h.cors(w, r) {
		return
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", "POST")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, err := h.auth.Authenticate(r)
	if err != nil || principal == nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if !principal.HasRole(httpapi.RoleView) {
		http.Error(w, "view role required", http.StatusForbidden)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		http.Error(w, "codec busy", http.StatusServiceUnavailable)
		return
	}
	h.transform(w, r)
}

func (h *httpHandler) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	w.Header().Add("Vary", "Origin")
	if !h.origins[origin] {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	return true
}

func (h *httpHandler) transform(w http.ResponseWriter, r *http.Request) {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		http.Error(w, "application/json required", http.StatusUnsupportedMediaType)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxHTTPBodyBytes))
	defer clear(body)
	if err != nil {
		http.Error(w, "invalid or oversized codec request", http.StatusBadRequest)
		return
	}
	var payloads commonpb.Payloads
	if err := protojson.Unmarshal(body, &payloads); err != nil {
		http.Error(w, "invalid codec request", http.StatusBadRequest)
		return
	}
	if r.URL.Path == "/encode" {
		payloads.Payloads, err = h.codec.EncodeContext(r.Context(), payloads.Payloads)
	} else {
		payloads.Payloads, err = h.codec.DecodeContext(r.Context(), payloads.Payloads)
	}
	if err != nil {
		http.Error(w, "codec operation failed", http.StatusBadRequest)
		return
	}
	response, err := protojson.Marshal(&payloads)
	defer clear(response)
	if err != nil || len(response) > MaxHTTPBodyBytes {
		http.Error(w, "codec operation failed", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(response)
}
