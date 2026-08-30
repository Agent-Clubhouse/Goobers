package stateclient

import (
	"bytes"
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

// Headers the scheduler-state plane speaks. Restated from the server for the
// same reason claimsclient restates the claims wire shapes: the stage-side
// client depends on the contract, not on the daemon's handler surface.
const (
	// HeaderETag carries the value's content digest on a read and on a write's
	// acknowledgement.
	HeaderETag = "ETag"
	// HeaderIfMatch carries the compare-and-swap precondition on a write.
	HeaderIfMatch = "If-Match"
	// HeaderIfNoneMatch carries the create-only precondition ("*"), the wire
	// spelling of an empty ifMatch.
	HeaderIfNoneMatch = "If-None-Match"
	// IfNoneMatchAny is the only If-None-Match value the plane accepts.
	IfNoneMatchAny = "*"
)

// Error is a typed refusal from the scheduler-state plane: the shared API
// error envelope's code and message beside the HTTP status.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("scheduler-state plane refused (%d %s): %s", e.Status, e.Code, e.Message)
}

// DefaultHTTPTimeout bounds one round trip, matching the claims plane's: a
// scheduler-state primitive that hangs delays a whole stage, and the daemon's
// own budget on these routes is the standard mutation budget.
const DefaultHTTPTimeout = 30 * time.Second

// HTTPConfig configures the scheduler-state plane backend.
type HTTPConfig struct {
	// BaseURL is the daemon API root (EnvEndpoint in the pod).
	BaseURL string
	// Token is the state-scoped bearer (EnvToken in the pod).
	Token string
	// Gaggle is the gaggle the caller acts for — the route's scope. The
	// daemon independently verifies that the calling pod's run belongs to it;
	// naming another gaggle here is refused, not served.
	Gaggle string
	// Client overrides the HTTP client; nil uses a bounded default.
	Client *http.Client
}

// HTTP is the scheduler-state plane backend.
type HTTP struct {
	cfg HTTPConfig
}

// NewHTTP constructs the plane backend.
func NewHTTP(cfg HTTPConfig) (*HTTP, error) {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		return nil, errors.New("stateclient: HTTP backend requires a base URL")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("stateclient: HTTP backend requires a bearer token")
	}
	if strings.TrimSpace(cfg.Gaggle) == "" {
		return nil, errors.New("stateclient: HTTP backend requires the caller's gaggle")
	}
	// The gaggle becomes a path SEGMENT. A value that is not one plain path
	// element (".", "..", anything carrying a separator) would be collapsed by
	// the HTTP client's path normalization into a request against a DIFFERENT
	// route — which answers the same 404 an absent key does, so a traversal
	// would read back as "no value here" rather than as a refusal. Fail
	// closed at construction, where it can still be told apart.
	if !plainPathElement(cfg.Gaggle) {
		return nil, fmt.Errorf("stateclient: %q is not a valid gaggle for the scheduler-state plane", cfg.Gaggle)
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	return &HTTP{cfg: cfg}, nil
}

// plainPathElement reports whether value is a single, ordinary path element.
func plainPathElement(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	return !strings.ContainsAny(value, `/\`)
}

// Gaggle reports the gaggle every call is scoped to.
func (h *HTTP) Gaggle() string { return h.cfg.Gaggle }

// endpoint builds the route URL. Both segments are path-escaped: the key has
// already been refused unless it is one of the closed namespace's shapes, and
// the gaggle comes from the stage's own environment, but a containment check
// must never resolve an input it has not encoded.
func (h *HTTP) endpoint(key string) string {
	path := apicontract.GaggleStateKeyPath
	path = strings.ReplaceAll(path, "{gaggle}", url.PathEscape(h.cfg.Gaggle))
	path = strings.ReplaceAll(path, "{key}", url.PathEscape(key))
	return h.cfg.BaseURL + path
}

// planeError decodes a non-success response into a typed refusal.
func planeError(status int, raw []byte) *Error {
	planeErr := &Error{Status: status}
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Code != "" {
		planeErr.Code, planeErr.Message = envelope.Error.Code, envelope.Error.Message
		return planeErr
	}
	detail := strings.TrimSpace(string(raw))
	if len(detail) > 400 {
		detail = detail[:400] + "…"
	}
	planeErr.Code, planeErr.Message = "http_"+fmt.Sprint(status), detail
	return planeErr
}

// Get implements Store over the route's read half. A 404 is the key's ABSENT
// state, not a failure: it answers the zero Value, so a first-run pod sees
// exactly what a first-run file backend sees.
func (h *HTTP) Get(ctx context.Context, key string) (Value, error) {
	if err := checkKey(key); err != nil {
		return Value{}, err
	}
	endpoint := h.endpoint(key)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Value{}, fmt.Errorf("stateclient: build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	response, err := h.cfg.Client.Do(request)
	if err != nil {
		return Value{}, fmt.Errorf("stateclient: %s: %w", endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxValueBytes+1))
	if err != nil {
		return Value{}, fmt.Errorf("stateclient: read response from %s: %w", endpoint, err)
	}
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return Value{}, nil
	default:
		return Value{}, planeError(response.StatusCode, raw)
	}
	if len(raw) > MaxValueBytes {
		return Value{}, fmt.Errorf("stateclient: %s answered more than the %d-byte value limit", endpoint, MaxValueBytes)
	}
	etag := strings.Trim(strings.TrimSpace(response.Header.Get(HeaderETag)), `"`)
	if etag == "" {
		// A served value with no ETag cannot be compare-and-swapped, so a
		// read-modify-write built on it would silently become a blind
		// overwrite. Fail closed rather than degrade to last-write-wins.
		return Value{}, fmt.Errorf("stateclient: %s answered a value without an %s header", endpoint, HeaderETag)
	}
	return Value{Data: raw, ETag: etag}, nil
}

// Put implements Store over the route's write half: If-Match for a replace,
// If-None-Match: * for a create. A 412 is ErrPreconditionFailed — the CAS
// lost, which Update retries.
func (h *HTTP) Put(ctx context.Context, key string, data []byte, ifMatch string) (Value, error) {
	if err := checkKey(key); err != nil {
		return Value{}, err
	}
	if err := checkValue(data); err != nil {
		return Value{}, err
	}
	endpoint := h.endpoint(key)
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(data))
	if err != nil {
		return Value{}, fmt.Errorf("stateclient: build request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	request.Header.Set("Content-Type", "application/json")
	if ifMatch == "" {
		request.Header.Set(HeaderIfNoneMatch, IfNoneMatchAny)
	} else {
		request.Header.Set(HeaderIfMatch, `"`+ifMatch+`"`)
	}
	response, err := h.cfg.Client.Do(request)
	if err != nil {
		return Value{}, fmt.Errorf("stateclient: %s: %w", endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return Value{}, fmt.Errorf("stateclient: read response from %s: %w", endpoint, err)
	}
	switch response.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return Value{Data: data, ETag: ETagFor(data)}, nil
	case http.StatusPreconditionFailed:
		return Value{}, ErrPreconditionFailed
	default:
		return Value{}, planeError(response.StatusCode, raw)
	}
}

// Section implements Store. There is no local lock to take: the plane's
// per-key lock lives in the daemon and is held for the duration of each
// individual request, not across fn. Isolation inside fn comes from the
// If-Match on every write it performs — an interleaved writer causes
// ErrPreconditionFailed, which the caller must surface rather than swallow.
func (h *HTTP) Section(ctx context.Context, key, _ string, fn func() error) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// Update implements Store as the compare-and-swap loop the plane's split
// read/write demands: read, apply fn, write under the read's ETag, and on a
// refusal re-read and re-apply. fn observing a value that changed under it is
// exactly the interleaving the file backend's single lock prevents — here the
// refused write is what prevents the lost update instead.
func (h *HTTP) Update(ctx context.Context, key, _ string, fn func(Value) ([]byte, bool, error)) error {
	if err := checkKey(key); err != nil {
		return err
	}
	for attempt := 0; attempt < MaxUpdateAttempts; attempt++ {
		current, err := h.Get(ctx, key)
		if err != nil {
			return err
		}
		data, write, err := fn(current)
		if err != nil || !write {
			return err
		}
		if _, err := h.Put(ctx, key, data, current.ETag); err != nil {
			if errors.Is(err, ErrPreconditionFailed) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("%w: key %s", ErrUpdateContention, key)
}
