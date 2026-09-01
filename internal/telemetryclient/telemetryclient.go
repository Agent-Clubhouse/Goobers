// Package telemetryclient is the stage-side reader for the daemon's
// gaggle-scoped telemetry read plane (decision 005 R4 / finding 002 C3).
//
// It exists for the same reason internal/claimsclient does: a stage running in
// a pod has no instance root, so evidence that used to come from the local
// telemetry rollup file has to come from the daemon instead. The selection is
// environment-driven and FAILS CLOSED — an endpoint without a bearer, or
// without the gaggle the read must be contained to, is an error, never a
// silent fall-through to a rollup file the pod does not have.
//
// Scope is deliberately narrow. This client reads DERIVED, low-sensitivity
// projections only: the implementation-outcome evidence
// `backlog-health --feedback` needs, and — since Goobers#4001, the blocker-1
// half of #3996 — the four fixed defect-nomination aggregates
// `telemetry-query` needs (defectaggregates.go). Neither read exposes the
// telemetry database, a query, a path, or a connector, and error signatures
// are NORMALIZED by the daemon before they cross.
//
// What is still not served here: raw telemetry events, the raw
// (code, error_class) signature route, and every EXTERNAL telemetry connector
// (executor.KindExternalTelemetry). A connector stage reaches a third-party
// vendor with the instance's own credential; that is not a derived aggregate
// and stays refused on the engine path.
package telemetryclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

// Environment variables that select the HTTP backend in a stage process.
const (
	// EnvEndpoint is the daemon API base URL the telemetry read plane is
	// reached at.
	//
	// Spelled telemetry-scoped rather than reusing the pod's control-plane
	// variables because __dispatch-exec STRIPS those from every stage's
	// environment (dispatcher.DispatcherPrivilegedEnv: a stage that can read
	// GOOBERS_POD_TOKEN can author its own outcome). A read-only, gaggle-
	// contained telemetry bearer is a different authority from the pod token
	// and travels in its own variable, exactly as the claims plane's does.
	EnvEndpoint = "GOOBERS_TELEMETRY_ENDPOINT"
	// EnvToken is the read-scoped bearer presented to the plane.
	EnvToken = "GOOBERS_TELEMETRY_TOKEN"
	// EnvGaggle is the gaggle the read is contained to — the plane's
	// containment key, checked server-side against the bearer's own run.
	EnvGaggle = "GOOBERS_GAGGLE"

	// EnvFallbackEndpoint and EnvFallbackToken are the pod's control-plane
	// spellings, accepted when the telemetry-scoped pair above is unset. They
	// are what an in-pod caller that has NOT been through the stage env
	// filter holds (dispatch-exec itself, and this issue's original wording:
	// "the CLI reads through it when GOOBERS_DAEMON_API is set"). A stage
	// subprocess never sees them, so accepting them widens nothing.
	EnvFallbackEndpoint = "GOOBERS_DAEMON_API"
	// EnvFallbackToken is the control-plane bearer paired with
	// EnvFallbackEndpoint.
	EnvFallbackToken = "GOOBERS_POD_TOKEN"
)

// ErrEndpointWithoutToken is the fail-closed refusal Select answers when an
// endpoint is configured but no bearer is: a stage in a pod must never fall
// back to a telemetry rollup file it does not have.
var ErrEndpointWithoutToken = errors.New("telemetryclient: a telemetry plane endpoint is set but its bearer token is empty; refusing to fall back to a local rollup")

// ErrEndpointWithoutGaggle is the sibling refusal for a missing gaggle. The
// plane contains a pod read to its own gaggle, so an unscoped read is refused
// here rather than sent to be refused there.
var ErrEndpointWithoutGaggle = errors.New("telemetryclient: a telemetry plane endpoint is set but " + EnvGaggle + " is empty; the plane contains every pod read to its own gaggle")

// DefaultTimeout bounds one round trip. Sized like the daemon's own bounded
// read budget plus margin: an evidence read that hangs delays a whole stage.
const DefaultTimeout = 30 * time.Second

// maxResponseBytes bounds a plane response so a misbehaving or compromised
// endpoint cannot exhaust a stage pod's memory. Generous relative to a
// gaggle's terminal implementation runs inside one ready-window.
const maxResponseBytes = 8 << 20

// Error is a typed refusal from the telemetry read plane: the shared API
// error envelope's code and message beside the HTTP status.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("telemetry read plane refused (%d %s): %s", e.Status, e.Code, e.Message)
}

// ImplementationOutcome is one terminal implementation run and the backlog
// item it claimed.
//
// Restated from internal/readservice (the server) tag for tag rather than
// imported, for the reason internal/claimsclient restates its wire shapes:
// the stage-side client depends on the CONTRACT, not on the daemon's read
// service. The server's tests pin the two against each other.
type ImplementationOutcome struct {
	RunID        string    `json:"runId"`
	ItemID       string    `json:"itemId"`
	Status       string    `json:"status"`
	StartedAt    time.Time `json:"startedAt"`
	FinishedAt   time.Time `json:"finishedAt"`
	Stage        string    `json:"stage,omitempty"`
	ErrorCode    string    `json:"errorCode,omitempty"`
	ErrorMessage string    `json:"errorMessage,omitempty"`
	Gate         string    `json:"gate,omitempty"`
	Verdict      string    `json:"verdict,omitempty"`
}

type implementationOutcomesResponse struct {
	Items []ImplementationOutcome `json:"items"`
}

// Config configures the read backend.
type Config struct {
	// BaseURL is the daemon API root.
	BaseURL string
	// Token is the bearer presented to the plane.
	Token string
	// Gaggle is the gaggle every read is scoped by.
	Gaggle string
	// Client overrides the HTTP client; nil uses a bounded default.
	Client *http.Client
}

// HTTP is the telemetry read plane backend.
type HTTP struct {
	cfg Config
}

// NewHTTP constructs the plane backend.
func NewHTTP(cfg Config) (*HTTP, error) {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		return nil, errors.New("telemetryclient: HTTP backend requires a base URL")
	}
	if err := ValidateEndpoint(cfg.BaseURL); err != nil {
		return nil, err
	}
	cfg.Token = strings.TrimSpace(cfg.Token)
	if cfg.Token == "" {
		return nil, ErrEndpointWithoutToken
	}
	cfg.Gaggle = strings.TrimSpace(cfg.Gaggle)
	if cfg.Gaggle == "" {
		return nil, ErrEndpointWithoutGaggle
	}
	if err := ValidateScopeName("gaggle", cfg.Gaggle); err != nil {
		return nil, err
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: DefaultTimeout}
	}
	return &HTTP{cfg: cfg}, nil
}

// ValidateEndpoint refuses a plane endpoint this client will not talk to.
//
// The endpoint arrives from the process environment, which in a stage pod is
// written by the dispatcher — but a bearer token is attached to every request
// made through it, so an endpoint that is not what it claims to be is a
// credential-exfiltration primitive, not merely a broken read. The rules are
// therefore structural and fail closed:
//
//   - an absolute http/https URL only: no file://, no gopher://, no
//     scheme-relative or opaque form that a URL parser and an HTTP client
//     might read differently;
//   - a host: an empty authority resolves against nothing predictable;
//   - NO embedded credentials, query string, or fragment: each of those is a
//     way to make the composed request address something other than the
//     contract path this client appends to it.
func ValidateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("telemetryclient: plane endpoint is not a valid URL: %w", err)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("telemetryclient: plane endpoint scheme %q is refused; only http and https are served", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("telemetryclient: plane endpoint has no host")
	}
	if parsed.User != nil {
		return errors.New("telemetryclient: plane endpoint must not embed credentials")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("telemetryclient: plane endpoint must not carry a query string or fragment")
	}
	if strings.Contains(parsed.Path, "..") {
		return errors.New("telemetryclient: plane endpoint path must not traverse")
	}
	return nil
}

// Select chooses the telemetry read backend for a stage process from its
// environment. It reports selected=false when no endpoint is configured — the
// caller then reads its own instance-local rollup, exactly as before — and
// fails closed on a configured endpoint with no bearer or no gaggle.
func Select(getenv func(string) string) (*HTTP, bool, error) {
	endpoint := strings.TrimSpace(getenv(EnvEndpoint))
	token := strings.TrimSpace(getenv(EnvToken))
	if endpoint == "" {
		endpoint = strings.TrimSpace(getenv(EnvFallbackEndpoint))
		token = strings.TrimSpace(getenv(EnvFallbackToken))
	}
	if endpoint == "" {
		return nil, false, nil
	}
	if token == "" {
		return nil, false, ErrEndpointWithoutToken
	}
	gaggle := strings.TrimSpace(getenv(EnvGaggle))
	if gaggle == "" {
		return nil, false, ErrEndpointWithoutGaggle
	}
	client, err := NewHTTP(Config{BaseURL: endpoint, Token: token, Gaggle: gaggle})
	if err != nil {
		return nil, false, err
	}
	return client, true, nil
}

// ImplementationOutcomes returns the terminal implementation runs in this
// client's gaggle that claimed a backlog item and started at or after since.
// A zero since is an unbounded window, matching the rollup query's own
// contract.
func (h *HTTP) ImplementationOutcomes(ctx context.Context, since time.Time) ([]ImplementationOutcome, error) {
	query := url.Values{}
	query.Set("gaggle", h.cfg.Gaggle)
	if !since.IsZero() {
		query.Set("since", since.UTC().Format(time.RFC3339Nano))
	}
	target := h.cfg.BaseURL + apicontract.TelemetryImplementationOutcomesPath + "?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("telemetryclient: build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	request.Header.Set("Accept", "application/json")

	response, err := h.cfg.Client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("telemetryclient: read implementation outcomes: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()
	if response.StatusCode != http.StatusOK {
		return nil, planeError(response)
	}
	var decoded implementationOutcomesResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxResponseBytes)).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("telemetryclient: decode implementation outcomes: %w", err)
	}
	return decoded.Items, nil
}

// planeError converts a non-200 into the typed refusal, preserving the
// envelope's code and message when the body carries one.
func planeError(response *http.Response) error {
	planeErr := &Error{Status: response.StatusCode, Code: "unknown", Message: http.StatusText(response.StatusCode)}
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil || len(body) == 0 {
		return planeErr
	}
	var envelope apicontract.ErrorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.Error.Code == "" {
		return planeErr
	}
	planeErr.Code = envelope.Error.Code
	planeErr.Message = envelope.Error.Message
	return planeErr
}
