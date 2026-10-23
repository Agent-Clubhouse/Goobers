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

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

// TerminalTokenMinter is deliberately separate from ordinary pod minting.
// Only the resident worker may use it; its bearer never enters an invocation.
type TerminalTokenMinter interface {
	MintWorkerTerminalIntervention(workerID string, ttl time.Duration) (string, error)
}

// CommitTerminalIntervention uses only the worker's dedicated credential.
// Token, which may be a pod bearer, is never a fallback for this authority.
// The Temporal activity owns retries so its pending delivery remains visible.
func (e *HTTPEmitter) CommitTerminalIntervention(ctx context.Context, req TerminalInterventionRequest) (journal.TerminalInterventionReceipt, error) {
	var out journal.TerminalInterventionReceipt
	minter, ok := e.Minter.(TerminalTokenMinter)
	if !ok || strings.TrimSpace(e.TerminalWorkerID) == "" || e.BaseURL == "" {
		return out, errors.New("livejournal: terminal intervention requires a dedicated worker minter, identity and endpoint")
	}
	if !apiv1.ValidRunID(req.RunID) {
		return out, journal.ErrInterventionInvalid
	}
	token, err := minter.MintWorkerTerminalIntervention(e.TerminalWorkerID, mintedTokenTTL)
	if err != nil {
		return out, fmt.Errorf("mint terminal intervention credential: %w", err)
	}
	if token == "" {
		return out, errors.New("livejournal: empty terminal intervention credential")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return out, err
	}
	endpoint := strings.TrimRight(e.BaseURL, "/") + "/api/v1/runs/" + url.PathEscape(req.RunID) + "/journal/terminal-intervention"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return out, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	client := http.Client{Timeout: emitHTTPTimeout}
	if e.Client != nil {
		client = *e.Client
	}
	// A redirect must never forward the privileged worker credential.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return out, err
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if err != nil {
		return out, err
	}
	if response.StatusCode != http.StatusOK {
		var envelope errorEnvelope
		_ = json.Unmarshal(payload, &envelope)
		refused := fmt.Errorf("livejournal: terminal intervention refused (%d %s): %s", response.StatusCode, envelope.Error.Code, envelope.Error.Message)
		switch response.StatusCode {
		case http.StatusConflict:
			return out, fmt.Errorf("%w: %w", journal.ErrInterventionConflict, refused)
		case http.StatusBadRequest:
			return out, fmt.Errorf("%w: %w", journal.ErrInterventionInvalid, refused)
		default:
			return out, refused
		}
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return out, fmt.Errorf("livejournal: decode terminal receipt: %w", err)
	}
	return out, nil
}

const emitHTTPTimeout = 30 * time.Second
