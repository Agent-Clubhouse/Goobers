package claimsclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/planehttp"
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
	Execution      bool   `json:"execution,omitempty"`
}

type claimListResponse struct {
	Entries         []Entry   `json:"entries"`
	History         []Entry   `json:"history,omitempty"`
	ClaimVisibility string    `json:"claimVisibility,omitempty"`
	ObservedAt      time.Time `json:"observedAt,omitzero"`
}

type claimRecoverRequest struct {
	RunID string `json:"runId"`
}

type claimRecoverResponse struct {
	Released []Entry `json:"released,omitempty"`
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
	cfg   HTTPConfig
	plane *planehttp.Client
}

// NewHTTP constructs the plane backend.
func NewHTTP(cfg HTTPConfig) (*HTTP, error) {
	return newHTTP(cfg, false)
}

func newHTTP(cfg HTTPConfig, anonymous bool) (*HTTP, error) {
	plane, err := planehttp.New(planehttp.Config{
		BaseURL:      cfg.BaseURL,
		Token:        cfg.Token,
		Client:       cfg.Client,
		Timeout:      DefaultHTTPTimeout,
		AllowNoToken: anonymous,
		BaseURLError: errors.New("claimsclient: HTTP backend requires a base URL"),
		TokenError:   errors.New("claimsclient: HTTP backend requires a bearer token"),
	})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.RunID) == "" {
		return nil, errors.New("claimsclient: HTTP backend requires the stage's run ID")
	}
	cfg.BaseURL = plane.BaseURL()
	cfg.Client = plane.HTTPClient()
	if cfg.MergeLockPoll <= 0 {
		cfg.MergeLockPoll = DefaultMergeLockPoll
	}
	if cfg.MergeLockLease <= 0 {
		cfg.MergeLockLease = DefaultMergeLockLease
	}
	return &HTTP{cfg: cfg, plane: plane}, nil
}

// post sends one claims-plane call. replaySafe is the route's declaration that
// replaying it after an ambiguous transport failure (stream reset, EOF) cannot
// change the outcome; see the per-route notes at each call site. Calls refused
// before a handler ran (admission, recovery) are always replayed.
func (h *HTTP) post(ctx context.Context, path string, body, target any, replaySafe bool) error {
	endpoint := h.cfg.BaseURL + path
	response, err := h.plane.DoJSONRetrying(ctx, http.MethodPost, path, body, nil, replaySafe)
	if err != nil {
		var requestErr *planehttp.RequestError
		if errors.As(err, &requestErr) && requestErr.Op == "encode" {
			return fmt.Errorf("claimsclient: encode request: %w", requestErr.Err)
		}
		if errors.As(err, &requestErr) && requestErr.Op == "build" {
			return fmt.Errorf("claimsclient: build request: %w", requestErr.Err)
		}
		return fmt.Errorf("claimsclient: %s: %w", endpoint, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := planehttp.ReadBounded(response.Body, 4<<20)
	if err != nil {
		return fmt.Errorf("claimsclient: read response from %s: %w", endpoint, err)
	}
	if response.StatusCode != http.StatusOK {
		return planehttp.DecodeError(response.StatusCode, raw, newPlaneError, planehttp.ErrorFallback{
			CodePrefix: "http_", DetailLimit: 400, Ellipsis: "…",
		})
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return fmt.Errorf("claimsclient: decode response from %s: %w", endpoint, err)
	}
	return nil
}

func newPlaneError(status int, code, message string) error {
	return &Error{Status: status, Code: code, Message: message}
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
	// Replay-safe: a same-run re-claim is the ledger's idempotent renew, and a
	// claim held by another run is refused identically however often it is asked.
	var response claimResponse
	if err := h.post(ctx, apicontract.ClaimAcquirePath, claimRequest{
		Gaggle: key.Gaggle, Provider: key.Provider, ItemID: key.ExternalID,
		RunID: runID, Workflow: workflow, LeaseSeconds: seconds,
	}, &response, true); err != nil {
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
	// Replay-safe: renew extends the run's own lease to now+lease, so a replay
	// only moves the expiry forward again; a lost lease answers ok=false.
	var response claimResponse
	if err := h.post(ctx, apicontract.ClaimRenewPath, claimRequest{
		Gaggle: key.Gaggle, Provider: key.Provider, ItemID: key.ExternalID,
		RunID: runID, Workflow: workflow, LeaseSeconds: seconds,
	}, &response, true); err != nil {
		return false, err
	}
	return response.Ok, nil
}

// ReleaseScoped implements Ledger over claims/release.
func (h *HTTP) ReleaseScoped(ctx context.Context, key Key, runID string) error {
	if err := scopedKey(key); err != nil {
		return err
	}
	// Replay-safe: release of a claim not held, or held by another run, is a
	// no-op in the ledger, so a replay cannot release a later holder's claim.
	var response claimResponse
	return h.post(ctx, apicontract.ClaimReleasePath, claimRequest{
		Gaggle: key.Gaggle, Provider: key.Provider, ItemID: key.ExternalID, RunID: runID,
	}, &response, true)
}

// ReleaseAllForRun implements Ledger over claims/release with itemId omitted.
func (h *HTTP) ReleaseAllForRun(ctx context.Context, runID string) ([]Entry, error) {
	// Not replay-safe after an ambiguous failure: the reply lists what THIS call
	// released, and a replay of a call that did commit answers an empty list.
	// It is still retried when the daemon refused it before a handler ran.
	var response claimResponse
	if err := h.post(ctx, apicontract.ClaimReleasePath, claimRequest{RunID: runID}, &response, false); err != nil {
		return nil, err
	}
	return response.Released, nil
}

// ForRunAll implements Ledger over claims/list scope=run.
func (h *HTTP) ForRunAll(ctx context.Context, runID string) ([]Entry, error) {
	var response claimListResponse
	if err := h.post(ctx, apicontract.ClaimListPath, claimListRequest{RunID: runID, Scope: scopeRun}, &response, true); err != nil {
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
	}, &response, true); err != nil {
		return Listing{}, err
	}
	return Listing{Entries: response.Entries, History: response.History}, nil
}

// ExecutionSnapshot reads only this run's leases, history and trusted pinned
// policy. Missing policy is refused: an older/unavailable server must never
// silently turn shared execution into local execution.
func (h *HTTP) ExecutionSnapshot(ctx context.Context) (string, Listing, error) {
	var response claimListResponse
	started := time.Now()
	if err := h.post(ctx, apicontract.ClaimListPath, claimListRequest{RunID: h.cfg.RunID, Scope: scopeRun, IncludeHistory: true, Execution: true}, &response, true); err != nil {
		return "", Listing{}, err
	}
	if response.ClaimVisibility != "local" && response.ClaimVisibility != "shared" {
		return "", Listing{}, fmt.Errorf("claims plane did not verify execution policy")
	}
	if response.ClaimVisibility == "shared" {
		if response.ObservedAt.IsZero() {
			return "", Listing{}, fmt.Errorf("claims plane omitted the execution observation clock")
		}
		// Anchor remaining server-clock authority at the START of the local
		// request. Network and server wait time are spent, never granted anew.
		// This remains conservative even when daemon and worker clocks differ.
		response.Entries = localExecutionTimes(response.Entries, started, response.ObservedAt)
		response.History = localExecutionTimes(response.History, started, response.ObservedAt)
	}
	return response.ClaimVisibility, Listing{Entries: response.Entries, History: response.History}, nil
}

func localExecutionTimes(entries []Entry, started, observed time.Time) []Entry {
	translate := func(value time.Time) time.Time {
		if value.IsZero() {
			return value
		}
		return started.Add(value.Sub(observed))
	}
	for i := range entries {
		entries[i].ExpiresAt = translate(entries[i].ExpiresAt)
		entries[i].SharedDeadline = translate(entries[i].SharedDeadline)
		entries[i].ClaimedAt = translate(entries[i].ClaimedAt)
		entries[i].RenewedAt = translate(entries[i].RenewedAt)
		if entries[i].ReleasedAt != nil {
			released := translate(*entries[i].ReleasedAt)
			entries[i].ReleasedAt = &released
		}
	}
	return entries
}

// errMergeLeaseLost is MergeLock's context.Cause when a renewal is
// DEFINITIVELY refused (RenewEntry's ok=false: the daemon says this run no
// longer holds the lease, not that the round trip failed) — distinct from a
// transient renewal transport error, which stays best-effort under the
// lease's own TTL exactly as before.
var errMergeLeaseLost = errors.New("claimsclient: merge lock lease lost to another run or expiry")

// MergeLock implements Ledger as a polled lease on the synthetic merge-lock
// item: acquire until held (the refusal's Holder is the wait signal), keep
// the lease renewed while fn runs, release on the way out. A holder that
// crashes mid-window leaks nothing: its lease lapses within the lease bound
// and the daemon's expiry reaper frees the item.
//
// fn runs under a lease-scoped context (leaseCtx below), not ctx directly:
// the moment a renewal comes back definitively refused, leaseCtx is
// cancelled with errMergeLeaseLost so fn fails closed on its next
// context-aware call instead of finishing the window unaware another run
// now holds it (a prior version discarded that refusal and let fn run to
// completion regardless).
func (h *HTTP) MergeLock(ctx context.Context, lock MergeLock, fn func(context.Context) error) error {
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
	leaseCtx, cancelLease := context.WithCancelCause(renewCtx)
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
				ok, err := h.renew(renewCtx, lock.Key, lock.RunID, lock.Workflow, h.cfg.MergeLockLease)
				if err != nil {
					// Best effort: a failed renewal shortens the window to
					// the remaining lease; the release below is what ends
					// it, same as an unreachable daemon always has.
					continue
				}
				if !ok {
					// Definitive: the daemon confirms this run no longer
					// holds the lease. Fail fn closed immediately rather
					// than let it keep acting on an exclusivity window that
					// already belongs to someone else, then stop renewing —
					// there is nothing left to keep alive.
					cancelLease(errMergeLeaseLost)
					return
				}
			}
		}
	}()
	fnErr := fn(leaseCtx)
	stopRenewing()
	cancelLease(context.Canceled)
	<-renewDone
	if fnErr == nil {
		if cause := context.Cause(leaseCtx); errors.Is(cause, errMergeLeaseLost) {
			fnErr = fmt.Errorf("merge lock %s: %w", lock.Key.ExternalID, cause)
		}
	}
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

// RecoverStale implements StaleRecoverer over claims/recover: the daemon runs
// its own sweep and answers with what it released.
func (h *HTTP) RecoverStale(ctx context.Context) ([]Entry, error) {
	// Replay-safe: the sweep releases whatever is stale at the time it runs, and
	// RecoverStale callers act on no individual entry of the reply.
	var response claimRecoverResponse
	if err := h.post(ctx, apicontract.ClaimRecoverPath, claimRecoverRequest{RunID: h.cfg.RunID}, &response, true); err != nil {
		return nil, err
	}
	return response.Released, nil
}

// Locked implements Ledger: no client-side lock exists on the plane — the
// daemon serializes every primitive under its own claims lock — so fn runs
// with each primitive as its own round trip.
func (h *HTTP) Locked(_ context.Context, _ string, fn func(Ledger) error) error {
	return fn(h)
}
