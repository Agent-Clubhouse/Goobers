package decider

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

const (
	systemOnePath   = "/v1/systemone"
	maxResponseSize = 1 << 20
	maxErrorBody    = 512
	maxBackoff      = 30 * time.Second
)

// Config configures a System One HTTP client. BaseURL, APIKey, and Model are
// required and have no defaults.
type Config struct {
	// BaseURL is the deployment host with no path suffix.
	BaseURL string
	// APIKey is sent as a bearer token and is never included in errors.
	APIKey string
	Model  string
	// HTTPClient defaults to a client with a 60s timeout that does not follow
	// redirects, so the bearer token is never replayed to another URL.
	HTTPClient *http.Client
	// MaxAttempts bounds tries on 429/529/5xx and transport errors. Default 3.
	MaxAttempts int
	// Backoff is the base delay, doubled per retry. Default 500ms.
	Backoff time.Duration
}

// Client calls a System One endpoint.
type Client struct {
	endpoint string
	key      string
	model    string
	http     *http.Client
	attempts int
	backoff  time.Duration
	sleep    func(context.Context, time.Duration) error
}

var _ Decider = (*Client)(nil)

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/"))
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") {
		return nil, errors.New("decider: BaseURL must be an absolute http(s) URL")
	}
	if base.Scheme == "http" && !isLoopback(base.Hostname()) {
		return nil, errors.New("decider: plain http is only allowed for loopback hosts")
	}
	if cfg.APIKey == "" || cfg.Model == "" {
		return nil, errors.New("decider: APIKey and Model are required")
	}
	c := &Client{
		endpoint: strings.TrimRight(base.String(), "/") + systemOnePath,
		key:      cfg.APIKey,
		model:    cfg.Model,
		http:     cfg.HTTPClient,
		attempts: cfg.MaxAttempts,
		backoff:  cfg.Backoff,
		sleep:    sleepCtx,
	}
	if c.http == nil {
		c.http = &http.Client{
			Timeout: 60 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if c.attempts <= 0 {
		c.attempts = 3
	}
	if c.backoff <= 0 {
		c.backoff = 500 * time.Millisecond
	}
	return c, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

type wireQuestion struct {
	Type         Kind `json:"type"`
	Instructions any  `json:"instructions"`
	Criteria     any  `json:"criteria,omitempty"`
}

type wireRequest struct {
	State     any                     `json:"state"`
	Model     string                  `json:"model"`
	Questions map[string]wireQuestion `json:"questions"`
}

// StatusError is a non-2xx response. Body is truncated and never contains the
// request or credentials.
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("decider: endpoint returned %d: %s", e.StatusCode, e.Body)
}

// Decide sends req and returns validated answers. A response that omits a
// question, changes its type, or returns a malformed answer is an error.
func (c *Client) Decide(ctx context.Context, req Request) (Response, error) {
	if len(req.Questions) == 0 || req.State == nil {
		return Response{}, errors.New("decider: state and at least one question are required")
	}
	wire := wireRequest{State: req.State, Model: c.model, Questions: make(map[string]wireQuestion, len(req.Questions))}
	for id, q := range req.Questions {
		if err := q.validate(); err != nil {
			return Response{}, fmt.Errorf("decider: question %q: %w", id, err)
		}
		wire.Questions[id] = wireQuestion{Type: q.Type, Instructions: q.Instructions, Criteria: q.criteria}
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return Response{}, fmt.Errorf("decider: encode request: %w", err)
	}
	var lastErr error
	for attempt := 0; attempt < c.attempts; attempt++ {
		if attempt > 0 {
			if err := c.sleep(ctx, c.delay(attempt)); err != nil {
				return Response{}, err
			}
		}
		resp, retry, err := c.once(ctx, body, req.Questions)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !retry || ctx.Err() != nil {
			break
		}
	}
	return Response{}, lastErr
}

// delay is the wait before retry attempt (1-based), doubling from the base
// and capped so a large MaxAttempts cannot overflow the shift.
func (c *Client) delay(attempt int) time.Duration {
	d := c.backoff
	for i := 1; i < attempt && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

func (c *Client) once(ctx context.Context, body []byte, asked map[string]Question) (Response, bool, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, false, fmt.Errorf("decider: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.key)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	res, err := c.http.Do(httpReq)
	if err != nil {
		// url.Error embeds the URL only, never headers; still drop the wrapper detail.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return Response{}, true, fmt.Errorf("decider: request failed: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseSize+1))
	if err != nil {
		return Response{}, true, fmt.Errorf("decider: read response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		snippet := string(data)
		if len(snippet) > maxErrorBody {
			snippet = snippet[:maxErrorBody]
		}
		retry := res.StatusCode == http.StatusTooManyRequests || res.StatusCode == 529 || res.StatusCode >= 500
		return Response{}, retry, &StatusError{StatusCode: res.StatusCode, Body: strings.TrimSpace(snippet)}
	}
	if len(data) > maxResponseSize {
		return Response{}, false, errors.New("decider: response exceeds size limit")
	}
	var out Response
	if err := json.Unmarshal(data, &out); err != nil {
		return Response{}, false, fmt.Errorf("decider: decode response: %w", err)
	}
	if err := checkAnswers(out, asked); err != nil {
		return Response{}, false, err
	}
	return out, false, nil
}

func checkAnswers(out Response, asked map[string]Question) error {
	for id, q := range asked {
		a, ok := out.Answers[id]
		if !ok {
			return fmt.Errorf("decider: response missing answer for %q", id)
		}
		if a.Type != q.Type {
			return fmt.Errorf("decider: answer %q has type %q, want %q", id, a.Type, q.Type)
		}
		switch a.Type {
		case KindNoul:
			if a.Yes == nil || *a.Yes < 0 || *a.Yes > 1 {
				return fmt.Errorf("decider: answer %q has no valid noul value", id)
			}
		case KindChoice:
			opts, _ := q.criteria.(map[string]any)
			if _, known := opts[a.Choice]; !known {
				return fmt.Errorf("decider: answer %q chose an option that was not offered", id)
			}
			if err := checkDistribution(id, a, opts); err != nil {
				return err
			}
		case KindScore:
			levels, _ := q.criteria.([]any)
			if a.Score == nil || *a.Score < 0 || *a.Score > float64(len(levels)-1) {
				return fmt.Errorf("decider: answer %q has no valid score", id)
			}
			if err := checkDistribution(id, a, levelKeys(len(levels))); err != nil {
				return err
			}
		}
	}
	return nil
}

// levelKeys is the set of probability keys a score answer may use: the level
// indices "0" through n-1.
func levelKeys(n int) map[string]any {
	keys := make(map[string]any, n)
	for i := range n {
		keys[strconv.Itoa(i)] = nil
	}
	return keys
}

// checkDistribution validates confidence and that probabilities form a
// distribution over allowed keys only.
func checkDistribution(id string, a Answer, allowed map[string]any) error {
	if a.Confidence == nil || *a.Confidence < 0 || *a.Confidence > 1 {
		return fmt.Errorf("decider: answer %q has no valid confidence", id)
	}
	var sum float64
	for k, p := range a.Probabilities {
		if _, ok := allowed[k]; !ok {
			return fmt.Errorf("decider: answer %q has a probability for an option that was not offered", id)
		}
		if p < 0 || p > 1 {
			return fmt.Errorf("decider: answer %q has an out-of-range probability", id)
		}
		sum += p
	}
	if len(a.Probabilities) == 0 || sum < 0.98 || sum > 1.02 {
		return fmt.Errorf("decider: answer %q probabilities sum to %s", id, strconv.FormatFloat(sum, 'f', 3, 64))
	}
	return nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
