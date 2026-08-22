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
// Token carries the pod's per-run bearer (internal/podauth) when the daemon
// requires authentication; empty is the loopback/no-auth posture. Transport
// failures and non-2xx statuses are plain errors — the engine's EmitJournal
// activity classifies every emitter failure as infrastructure and retries on
// its bounded budget, and the writer's idempotency keys make redelivery safe.
type HTTPEmitter struct {
	// BaseURL is the daemon API root, e.g. "http://127.0.0.1:7777".
	BaseURL string
	// Token is the bearer presented as Authorization; empty sends none.
	Token string
	// Client overrides the HTTP client; nil uses a 30s-timeout default.
	Client *http.Client
}

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
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return EmitResponse{}, fmt.Errorf("livejournal: build emit request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if e.Token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+e.Token)
	}
	client := e.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return EmitResponse{}, fmt.Errorf("livejournal: emit to %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return EmitResponse{}, fmt.Errorf("livejournal: read emit response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var envelope errorEnvelope
		if json.Unmarshal(payload, &envelope) == nil && envelope.Error.Code != "" {
			return EmitResponse{}, fmt.Errorf("livejournal: emit refused (%d %s): %s", resp.StatusCode, envelope.Error.Code, envelope.Error.Message)
		}
		return EmitResponse{}, fmt.Errorf("livejournal: emit refused with status %d", resp.StatusCode)
	}
	var out EmitResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return EmitResponse{}, fmt.Errorf("livejournal: decode emit response: %w", err)
	}
	return out, nil
}
