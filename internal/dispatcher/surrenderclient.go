package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// surrenderclient.go is the pod-side half of the surrender plane's network
// transport (#3699): the same "PUT identity-keyed data to the daemon write
// API, authenticated with the pod's per-run bearer" shape
// internal/livejournal.HTTPEmitter already ships for journal emission, and
// the same network-plane pattern decision 010 already ships for artifacts
// (blob.go's BlobClient) — never a shared PVC, never a dispatcher-brokered
// byte path. cmd/goobers's dispatch-exec entrypoint is the only caller.

// errorEnvelope mirrors apicontract.ErrorEnvelope without importing it, the
// same tradeoff internal/livejournal.HTTPEmitter makes: this package stays
// beneath the API layers.
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// defaultSurrenderTimeout bounds one surrender PUT attempt when no client is
// supplied.
const defaultSurrenderTimeout = 30 * time.Second

// defaultSurrenderRetryDeadline bounds the whole surrender retry loop when
// the caller sets no RetryDeadline of its own. dispatchexec.go always sets
// one, scaled to the stage's own timeout (#4260); this is the floor a
// caller that skips that gets instead of no retry at all.
const defaultSurrenderRetryDeadline = 10 * time.Minute

// SurrenderPutClient PUTs one attempt's surrendered result to the daemon
// write API's surrender plane over HTTP.
type SurrenderPutClient struct {
	// BaseURL is the daemon API root, e.g. "http://127.0.0.1:7777"
	// (GOOBERS_DAEMON_API in the pod).
	BaseURL string
	// Token is the bearer presented as Authorization; empty sends none
	// (GOOBERS_POD_TOKEN in the pod).
	Token string
	// Client overrides the HTTP client; nil uses a 30s-timeout default.
	Client *http.Client
	// RetryDeadline bounds how long Put retries a transport error or 5xx
	// response before giving up. Zero uses defaultSurrenderRetryDeadline.
	// The endpoint is a write-once upsert keyed by (runID, stage, attempt)
	// (surrender.go:298-302), so retrying the identical body is always safe.
	RetryDeadline time.Duration
}

// Put posts one attempt's surrendered result document, retrying a transport
// error or 5xx response with jittered backoff until it succeeds, hits a
// non-retryable refusal, or RetryDeadline elapses.
func (c *SurrenderPutClient) Put(ctx context.Context, runID, stage string, attempt int, data []byte) error {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return errors.New("dispatcher: surrender client has no base URL")
	}
	if runID == "" || stage == "" || attempt < 1 {
		return fmt.Errorf("dispatcher: surrender identity requires run, stage, and a positive attempt (got run %q stage %q attempt %d)", runID, stage, attempt)
	}
	endpoint := base + "/api/v1/runs/" + url.PathEscape(runID) + "/stages/" + url.PathEscape(stage) + "/attempts/" + strconv.Itoa(attempt) + "/surrender"
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: defaultSurrenderTimeout}
	}
	deadline := c.RetryDeadline
	if deadline <= 0 {
		deadline = defaultSurrenderRetryDeadline
	}
	return withRetry(ctx, deadline, func(ctx context.Context) (bool, error) {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			return false, fmt.Errorf("dispatcher: build surrender request: %w", err)
		}
		request.Header.Set("Content-Type", "application/json")
		if c.Token != "" {
			request.Header.Set("Authorization", "Bearer "+c.Token)
		}
		resp, err := client.Do(request)
		if err != nil {
			return true, fmt.Errorf("dispatcher: surrender PUT to %s: %w", endpoint, err)
		}
		defer func() { _ = resp.Body.Close() }()
		payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return true, fmt.Errorf("dispatcher: read surrender response: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			retryable := retryableStatus(resp.StatusCode)
			var envelope errorEnvelope
			if json.Unmarshal(payload, &envelope) == nil && envelope.Error.Code != "" {
				return retryable, fmt.Errorf("dispatcher: surrender refused (%d %s): %s", resp.StatusCode, envelope.Error.Code, envelope.Error.Message)
			}
			return retryable, fmt.Errorf("dispatcher: surrender refused with status %d", resp.StatusCode)
		}
		return false, nil
	})
}
