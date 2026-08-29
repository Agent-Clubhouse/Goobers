package claimsclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
)

// Wire shapes restated from internal/httpapi (the server), tag for tag; the
// server's tests pin them against the originals. Restated rather than
// imported so the stage-side client depends on the contract's paths, not on
// the daemon's handler surface (the same reason internal/dispatcher restates
// MintedCredential).
type claimRequest struct {
	Gaggle       string `json:"gaggle,omitempty"`
	Provider     string `json:"provider,omitempty"`
	ItemID       string `json:"itemId,omitempty"`
	RunID        string `json:"runId"`
	Workflow     string `json:"workflow,omitempty"`
	LeaseSeconds int    `json:"leaseSeconds,omitempty"`
}

type claimResponse struct {
	Ok        bool       `json:"ok"`
	Holder    string     `json:"holder,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	Released  []Entry    `json:"released,omitempty"`
}

type claimListRequest struct {
	Gaggle         string `json:"gaggle,omitempty"`
	Provider       string `json:"provider,omitempty"`
	RunID          string `json:"runId"`
	Scope          string `json:"scope"`
	IncludeHistory bool   `json:"includeHistory,omitempty"`
}

type claimListResponse struct {
	Entries []Entry `json:"entries"`
	History []Entry `json:"history,omitempty"`
}

// List scopes, restated from the server.
const (
	scopeRun       = "run"
	scopeNamespace = "namespace"
)

// Error is a typed refusal from the claims plane: the shared API error
// envelope's code and message beside the HTTP status.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("claims plane refused (%d %s): %s", e.Status, e.Code, e.Message)
}

// Defaults for the HTTP backend.
const (
	// DefaultHTTPTimeout bounds one round trip. Short on purpose: a claim
	// primitive that hangs delays a whole stage, and the daemon's own budget
	// on these routes is 8s (apicontract.MutationBudget).
	DefaultHTTPTimeout = 30 * time.Second
	// DefaultMergeLockPoll is how often a waiting merge-lock claimant retries
	// acquire while another run holds the window.
	DefaultMergeLockPoll = 2 * time.Second
	// DefaultMergeLockLease bounds the merge window's lease; renewed while fn
	// runs, and the time a crashed holder's lock takes to lapse on its own.
	DefaultMergeLockLease = 10 * time.Minute
)

// HTTPConfig configures the claims-plane backend.
type HTTPConfig struct {
	// BaseURL is the daemon API root (EnvEndpoint in the pod).
	BaseURL string
	// Token is the claims-scoped bearer (EnvToken in the pod).
	Token string
	// RunID is the stage's own run — the plane's containment key on every
	// call, including namespace listings.
	RunID string
	// Client overrides the HTTP client; nil uses a bounded default.
	Client *http.Client
	// MergeLockPoll and MergeLockLease override the merge-lock defaults.
	MergeLockPoll  time.Duration
	MergeLockLease time.Duration
}

// HTTP is the claims-plane backend.
type HTTP struct {
	cfg HTTPConfig
}

// NewHTTP constructs the plane backend.
func NewHTTP(cfg HTTPConfig) (*HTTP, error) {
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		return nil, errors.New("claimsclient: HTTP backend requires a base URL")
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("claimsclient: HTTP backend requires a bearer token")
	}
	if strings.TrimSpace(cfg.RunID) == "" {
		return nil, errors.New("claimsclient: HTTP backend requires the stage's run ID")
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	if cfg.MergeLockPoll <= 0 {
		cfg.MergeLockPoll = DefaultMergeLockPoll
	}
	if cfg.MergeLockLease <= 0 {
		cfg.MergeLockLease = DefaultMergeLockLease
	}
	return &HTTP{cfg: cfg}, nil
}

func (h *HTTP) post(ctx context.Context, path string, body, target any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("claimsclient: encode request: %w", err)
	}
	endpoint := h.cfg.BaseURL + path
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("claimsclient: build request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+h.cfg.Token)
	response, err := h.cfg.Client.Do(request)
	if err != nil {
		return fmt.Errorf("claimsclient: %s: %w", endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("claimsclient: read response from %s: %w", endpoint, err)
	}
	if response.StatusCode != http.StatusOK {
		planeErr := &Error{Status: response.StatusCode}
		var envelope struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &envelope) == nil && envelope.Error.Code != "" {
			planeErr.Code, planeErr.Message = envelope.Error.Code, envelope.Error.Message
		} else {
			detail := strings.TrimSpace(string(raw))
			if len(detail) > 400 {
				detail = detail[:400] + "…"
			}
			planeErr.Code, planeErr.Message = "http_"+fmt.Sprint(response.StatusCode), detail
		}
		return planeErr
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("claimsclient: decode response from %s: %w", endpoint, err)
	}
	return nil
}

func scopedKey(key Key) error {
	if key.Gaggle == "" || key.Provider == "" {
		return ErrLegacyKeyOverPlane
	}
	if key.ExternalID == "" {
		return errors.New("claimsclient: claim key requires an item ID")
	}
	return nil
}

// ClaimScoped implements Ledger over claims/acquire.
func (h *HTTP) ClaimScoped(ctx context.Context, key Key, runID, workflow string, lease time.Duration) (bool, string, error) {
	if err := scopedKey(key); err != nil {
		return false, "", err
	}
	seconds, err := leaseSeconds(lease)
	if err != nil {
		return false, "", err
	}
	var response claimResponse
	if err := h.post(ctx, apicontract.ClaimAcquirePath, claimRequest{
		Gaggle: key.Gaggle, Provider: key.Provider, ItemID: key.ExternalID,
		RunID: runID, Workflow: workflow, LeaseSeconds: seconds,
	}, &response); err != nil {
		return false, "", err
	}
	if !response.Ok {
		return false, response.Holder, nil
	}
	return true, runID, nil
}

// renew extends the run's own lease on key (claims/renew); ok=false reports
// a lease that is no longer the run's to renew.
func (h *HTTP) renew(ctx context.Context, key Key, runID, workflow string, lease time.Duration) (bool, error) {
	seconds, err := leaseSeconds(lease)
	if err != nil {
		return false, err
	}
	var response claimResponse
	if err := h.post(ctx, apicontract.ClaimRenewPath, claimRequest{
		Gaggle: key.Gaggle, Provider: key.Provider, ItemID: key.ExternalID,
		RunID: runID, Workflow: workflow, LeaseSeconds: seconds,
	}, &response); err != nil {
		return false, err
	}
	return response.Ok, nil
}

// ReleaseScoped implements Ledger over claims/release.
func (h *HTTP) ReleaseScoped(ctx context.Context, key Key, runID string) error {
	if err := scopedKey(key); err != nil {
		return err
	}
	var response claimResponse
	return h.post(ctx, apicontract.ClaimReleasePath, claimRequest{
		Gaggle: key.Gaggle, Provider: key.Provider, ItemID: key.ExternalID, RunID: runID,
	}, &response)
}

// ReleaseAllForRun implements Ledger over claims/release with itemId omitted.
func (h *HTTP) ReleaseAllForRun(ctx context.Context, runID string) ([]Entry, error) {
	var response claimResponse
	if err := h.post(ctx, apicontract.ClaimReleasePath, claimRequest{RunID: runID}, &response); err != nil {
		return nil, err
	}
	return response.Released, nil
}

// ForRunAll implements Ledger over claims/list scope=run.
func (h *HTTP) ForRunAll(ctx context.Context, runID string) ([]Entry, error) {
	var response claimListResponse
	if err := h.post(ctx, apicontract.ClaimListPath, claimListRequest{RunID: runID, Scope: scopeRun}, &response); err != nil {
		return nil, err
	}
	return response.Entries, nil
}

// ListNamespace implements Ledger over claims/list scope=namespace, always
// with history: the plane's one namespace read serves both the holder
// filters and the failure-streak deprioritization.
func (h *HTTP) ListNamespace(ctx context.Context, gaggle, provider string) (Listing, error) {
	if gaggle == "" || provider == "" {
		return Listing{}, ErrLegacyKeyOverPlane
	}
	var response claimListResponse
	if err := h.post(ctx, apicontract.ClaimListPath, claimListRequest{
		Gaggle: gaggle, Provider: provider, RunID: h.cfg.RunID, Scope: scopeNamespace, IncludeHistory: true,
	}, &response); err != nil {
		return Listing{}, err
	}
	return Listing{Entries: response.Entries, History: response.History}, nil
}

// MergeLock implements Ledger as a polled lease on the synthetic merge-lock
// item: acquire until held (the refusal's Holder is the wait signal), keep
// the lease renewed while fn runs, release on the way out. A holder that
// crashes mid-window leaks nothing: its lease lapses within the lease bound
// and the daemon's expiry reaper frees the item.
func (h *HTTP) MergeLock(ctx context.Context, lock MergeLock, fn func() error) error {
	if err := scopedKey(lock.Key); err != nil {
		return err
	}
	if lock.RunID == "" {
		return errors.New("claimsclient: merge lock requires the holder's run ID")
	}
	holder := ""
	for {
		ok, refusedBy, err := h.ClaimScoped(ctx, lock.Key, lock.RunID, lock.Workflow, h.cfg.MergeLockLease)
		if err != nil {
			if ctx.Err() != nil && holder != "" {
				// The wait ran out mid-poll: name the holder we were waiting
				// on, not the transport's view of the cancelled round trip.
				return fmt.Errorf("acquire merge lock %s: held by run %s: %w", lock.Key.ExternalID, holder, ctx.Err())
			}
			return fmt.Errorf("acquire merge lock %s: %w", lock.Key.ExternalID, err)
		}
		if ok {
			break
		}
		holder = refusedBy
		select {
		case <-ctx.Done():
			return fmt.Errorf("acquire merge lock %s: held by run %s: %w", lock.Key.ExternalID, holder, ctx.Err())
		case <-time.After(h.cfg.MergeLockPoll):
		}
	}
	renewCtx, stopRenewing := context.WithCancel(ctx)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		ticker := time.NewTicker(h.cfg.MergeLockLease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				// Best effort: a failed renewal shortens the window to the
				// remaining lease; the release below is what ends it.
				_, _ = h.renew(renewCtx, lock.Key, lock.RunID, lock.Workflow, h.cfg.MergeLockLease)
			}
		}
	}()
	fnErr := fn()
	stopRenewing()
	<-renewDone
	// Release on a context that outlives a cancelled fn: the window must be
	// handed back even when the stage is being torn down.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), DefaultHTTPTimeout)
	defer cancel()
	if err := h.ReleaseScoped(releaseCtx, lock.Key, lock.RunID); err != nil {
		return errors.Join(fnErr, fmt.Errorf("release merge lock %s: %w", lock.Key.ExternalID, err))
	}
	return fnErr
}

// ContainedRunID implements Contained: the plane admits this bearer for one
// run only.
func (h *HTTP) ContainedRunID() string { return h.cfg.RunID }

// Locked implements Ledger: no client-side lock exists on the plane — the
// daemon serializes every primitive under its own claims lock — so fn runs
// with each primitive as its own round trip.
func (h *HTTP) Locked(_ context.Context, _ string, fn func(Ledger) error) error {
	return fn(h)
}
