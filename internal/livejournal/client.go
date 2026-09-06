package livejournal

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
)

// HTTPEmitter emits journal batches to the daemon write API's journal plane
// (POST /api/v1/runs/{run}/journal/emit) — the remote half of the emission
// seam. The daemon's own in-process emitters bypass HTTP by using *Writer
// directly; a stage-executing worker outside the daemon pod uses this.
//
// Token carries a fixed bearer when the caller already holds one (a stage pod
// is handed its own per-run token). A WORKER holds no such token: it serves
// many runs and is not itself a pod, so it carries the signing KEY instead and
// mints per batch — set Minter for that. With neither, no Authorization header
// is sent, which is the loopback/no-auth posture only. A transport error or
// 5xx response is retried with jittered backoff (RetryDeadline, #4260) before
// surfacing as a plain error; a non-retryable refusal (4xx) surfaces
// immediately. The engine's EmitJournal activity additionally classifies
// every emitter failure that does surface as infrastructure and retries on
// its own bounded budget, and the writer's idempotency keys make any of this
// redelivery safe — a retried or replayed batch can only be a no-op on the
// server side.
type HTTPEmitter struct {
	// BaseURL is the daemon API root, e.g. "http://127.0.0.1:7777".
	BaseURL string
	// Token is the bearer presented as Authorization; empty sends none.
	Token string
	// Minter, used only when Token is empty, issues a bearer scoped to the run
	// each batch belongs to. This is the worker's posture: it holds the shared
	// signing key (the same one it uses to mint stage-pod tokens) rather than
	// any single run's token, so the credential is derived per batch from the
	// run being emitted for — never a long-lived ambient one.
	Minter TokenMinter
	// Client overrides the HTTP client; nil uses a 30s-timeout default.
	Client *http.Client
	// RetryDeadline bounds how long Emit retries a transport error or 5xx
	// response before giving up. Zero uses defaultEmitRetryDeadline. Safe to
	// retry unconditionally: every op carries an idempotency key the writer
	// dedupes on, so a redelivered batch cannot double-apply.
	RetryDeadline time.Duration
}

// TokenMinter issues a per-run bearer. Declared here as a SEAM rather than
// importing internal/podauth, which would invert the layering — this package
// stays beneath the API layers. dispatcher.TokenMinter is the same shape for
// the same reason, and podauth.SignedKey satisfies both.
type TokenMinter interface {
	Mint(runID string, ttl time.Duration) (string, error)
}

// mintedTokenTTL bounds a minted journal bearer. Short because it is used
// immediately by the request that mints it; it is not stored, reused across
// batches, or handed to another process.
const mintedTokenTTL = 5 * time.Minute

// errorEnvelope mirrors apicontract.ErrorEnvelope without importing it (this
// package stays beneath the API layers).
type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Emit posts one batch.
func (e *HTTPEmitter) Emit(ctx context.Context, req EmitRequest) (EmitResponse, error) {
	base := strings.TrimRight(e.BaseURL, "/")
	if base == "" {
		return EmitResponse{}, errors.New("livejournal: HTTP emitter has no base URL")
	}
	if req.RunID == "" {
		return EmitResponse{}, errors.New("livejournal: emit request has no run id")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return EmitResponse{}, fmt.Errorf("livejournal: marshal emit request: %w", err)
	}
	endpoint := base + "/api/v1/runs/" + url.PathEscape(req.RunID) + "/journal/emit"
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	deadline := e.RetryDeadline
	if deadline <= 0 {
		deadline = defaultEmitRetryDeadline
	}
	var out EmitResponse
	err = withRetry(ctx, deadline, func(ctx context.Context) (bool, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return false, fmt.Errorf("livejournal: build emit request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		switch {
		case e.Token != "":
			httpReq.Header.Set("Authorization", "Bearer "+e.Token)
		case e.Minter != nil:
			// Scoped to THIS batch's run, so a worker serving many runs never
			// presents one run's authority while emitting for another. Minted
			// fresh on every attempt: a short-TTL bearer minted once at the
			// top of a multi-minute retry loop could expire before the
			// retry that finally lands.
			token, mErr := e.Minter.Mint(req.RunID, mintedTokenTTL)
			if mErr != nil {
				return false, fmt.Errorf("livejournal: mint bearer for run %s: %w", req.RunID, mErr)
			}
			httpReq.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(httpReq)
		if err != nil {
			return true, fmt.Errorf("livejournal: emit to %s: %w", endpoint, err)
		}
		defer func() { _ = resp.Body.Close() }()
		payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil {
			return true, fmt.Errorf("livejournal: read emit response: %w", err)
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			retryable := retryableStatus(resp.StatusCode)
			var envelope errorEnvelope
			if json.Unmarshal(payload, &envelope) == nil && envelope.Error.Code != "" {
				return retryable, fmt.Errorf("livejournal: emit refused (%d %s): %s", resp.StatusCode, envelope.Error.Code, envelope.Error.Message)
			}
			return retryable, fmt.Errorf("livejournal: emit refused with status %d", resp.StatusCode)
		}
		if err := json.Unmarshal(payload, &out); err != nil {
			return false, fmt.Errorf("livejournal: decode emit response: %w", err)
		}
		return false, nil
	})
	if err != nil {
		return EmitResponse{}, err
	}
	return out, nil
}
