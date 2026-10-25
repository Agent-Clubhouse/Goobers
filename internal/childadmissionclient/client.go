// Package childadmissionclient is the engine worker's client for the engine
// child-admission plane (#7879). Workers hold no database credentials; the
// control plane owns generated-child custody and serves it over this route.
package childadmissionclient

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

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/childresult"
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
)

const (
	defaultTimeout   = 30 * time.Second
	maxReceiptBytes  = 64 << 10
	maxManifestReply = httpapi.MaxEngineChildManifestBytes*4/3 + 4<<10
)

// Client implements engine.ChildService over the plane. TokenSource mints a
// short-lived worker child-admission credential per request.
type Client struct {
	BaseURL     string
	TokenSource func() (string, error)
	HTTP        *http.Client
	// PodEndpoint is where stage pods reach control; an authority request that
	// names none is sent with it.
	PodEndpoint string
	// Secrets registers every acquired child-workflow bearer with the worker's
	// exact-value scrubber; required to acquire authority.
	Secrets interface{ RegisterUntil([]byte, time.Time) }
}

var _ engine.ChildService = (*Client)(nil)

// workerTokenTTL is each call's bearer lifetime.
const workerTokenTTL = 2 * time.Minute

// ForWorker is the engine worker's generated-child custody client. It mints a
// fresh instance-wide child-admission bearer per call from the
// supervisor-loaded signing key; neither the key nor a bearer enters the
// worker environment, which stage processes inherit. It returns an untyped nil
// without a key or endpoint, so the engine refuses child custody.
func ForWorker(baseURL, owner string, signer *podauth.SignedKey) engine.ChildService {
	if signer == nil || baseURL == "" {
		return nil
	}
	return &Client{BaseURL: baseURL, TokenSource: func() (string, error) { return signer.MintWorkerChildAdmission(owner, workerTokenTTL) }}
}

// Admit records one idempotent child start intent.
func (c *Client) Admit(ctx context.Context, in engine.ChildAdmissionInput) (engine.ChildRunBinding, error) {
	var out engine.ChildRunBinding
	return out, c.call(ctx, apicontract.EngineChildAdmitPath, in, http.StatusOK, maxReceiptBytes, &out)
}

// Load returns the exact frozen manifest bytes; the engine verifies digests.
func (c *Client) Load(ctx context.Context, b engine.ChildRunBinding) ([]byte, error) {
	var out httpapi.EngineChildManifest
	if err := c.call(ctx, apicontract.EngineChildLoadPath, b, http.StatusOK, maxManifestReply, &out); err != nil {
		return nil, err
	}
	return out.Manifest, nil
}

// Cancel fences the family. It is idempotent on the control plane.
func (c *Client) Cancel(ctx context.Context, b engine.ChildRunBinding) error {
	return c.call(ctx, apicontract.EngineChildCancelPath, b, http.StatusNoContent, 0, nil)
}

// AuthorityForWorker is the engine worker's client for its stage attempts'
// child-workflow grants, over the same plane and credential as ForWorker.
// podEndpoint is where the worker's stage pods reach control, and secrets the
// worker's shared exact-value registry, so the journal scrubber and the
// dispatch canary know every bearer. It returns an untyped nil without any of
// them, so the engine refuses child-capable stages.
func AuthorityForWorker(baseURL, podEndpoint, owner string, signer *podauth.SignedKey, secrets interface{ RegisterUntil([]byte, time.Time) }) engine.ChildWorkflowAuthority {
	if signer == nil || baseURL == "" || podEndpoint == "" || secrets == nil {
		return nil
	}
	return &Client{BaseURL: baseURL, PodEndpoint: podEndpoint, Secrets: secrets,
		TokenSource: func() (string, error) { return signer.MintWorkerChildAdmission(owner, workerTokenTTL) }}
}

var _ engine.ChildWorkflowAuthority = (*Client)(nil)

// maxGrantReply bounds an authority response: one grant and its metadata.
const maxGrantReply = 16 << 10

// AcquireChildAuthority asks control to bind (or re-deliver) the attempt's
// grant. The returned grant carries a bearer.
func (c *Client) AcquireChildAuthority(ctx context.Context, request engine.ChildAuthorityRequest) (engine.ChildAuthorityGrant, error) {
	if c.Secrets == nil {
		return engine.ChildAuthorityGrant{}, errors.New("childadmissionclient: child authority requires a secret registry")
	}
	if request.Endpoint == "" {
		request.Endpoint = c.PodEndpoint
	}
	if err := request.Validate(); err != nil {
		return engine.ChildAuthorityGrant{}, fmt.Errorf("%w: %w", engine.ErrChildCustodyRefused, err)
	}
	var grant engine.ChildAuthorityGrant
	if err := c.call(ctx, apicontract.EngineChildAuthorityPath, request, http.StatusOK, maxGrantReply, &grant); err != nil {
		return engine.ChildAuthorityGrant{}, err
	}
	if _, err := grant.Access(request.RunID); err != nil || grant.GrantID == "" {
		return engine.ChildAuthorityGrant{}, errors.Join(errors.New("childadmissionclient: control returned an invalid child-workflow grant"), err)
	}
	c.Secrets.RegisterUntil([]byte(grant.BearerToken), grant.ExpiresAt)
	return grant, nil
}

// ReleaseChildAuthority asks control to revoke exactly the named grant.
func (c *Client) ReleaseChildAuthority(ctx context.Context, release engine.ChildAuthorityRelease) error {
	if err := release.Validate(); err != nil {
		return fmt.Errorf("%w: %w", engine.ErrChildCustodyRefused, err)
	}
	return c.call(ctx, apicontract.EngineChildAuthorityReleasePath, release, http.StatusNoContent, 0, nil)
}

// maxResultReply bounds a sealed child result: its bounded summary and
// references, plus identity.
const maxResultReply = 64 << 10

// Park lends the parked parent's capacity back to control's scheduler.
func (c *Client) Park(ctx context.Context, in engine.ChildParkInput) error {
	return c.call(ctx, apicontract.EngineChildParkPath, in, http.StatusNoContent, 0, nil)
}

// Finalize asks control to seal the child's result from its journal.
func (c *Client) Finalize(ctx context.Context, in engine.ChildFinalizeInput) (childresult.TerminalReturn, error) {
	var out childresult.TerminalReturn
	return out, c.call(ctx, apicontract.EngineChildFinalizePath, in, http.StatusOK, maxResultReply, &out)
}

// Complete asks control to verify the sealed result, reacquire the parent's
// capacity and return its continuation; the engine validates the pointer.
func (c *Client) Complete(ctx context.Context, in engine.ChildCompletionInput) (apiv1.ContextPointer, error) {
	var out apiv1.ContextPointer
	return out, c.call(ctx, apicontract.EngineChildCompletePath, in, http.StatusOK, maxReceiptBytes, &out)
}

func (c *Client) call(ctx context.Context, path string, body any, want int, limit int64, out any) error {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" || c.TokenSource == nil {
		return errors.New("childadmissionclient: plane endpoint and worker credential are required")
	}
	token, err := c.TokenSource()
	if err != nil || token == "" {
		return errors.Join(errors.New("childadmissionclient: worker credential unavailable"), err)
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: defaultTimeout}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	// Keep the worker credential on the configured endpoint.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("childadmissionclient: %s: %w", path, err)
		}
		// The plane may be mid-failover; the engine retries for its budget.
		return fmt.Errorf("childadmissionclient: %s: %w: %w", path, engine.ErrChildCustodyUnavailable, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != want {
		var envelope apicontract.ErrorEnvelope
		_ = json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&envelope)
		failure := fmt.Errorf("childadmissionclient: %s refused with status %d (%s): %s", path, response.StatusCode, envelope.Error.Code, envelope.Error.Message)
		if class := statusClass(response.StatusCode); class != nil {
			return fmt.Errorf("%w: %w", class, failure)
		}
		return failure
	}
	if out == nil {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errors.New("childadmissionclient: response exceeds its byte budget")
	}
	return json.Unmarshal(data, out)
}

// statusClass maps a plane status onto the engine's custody error classes.
// Only the plane's explicit 409 refusal is permanent. 503 (no controller holds
// custody), gateway failures, 429, and 404/405 (the route is missing while an
// API rollout is in progress) are retryable. Everything else, including
// credential failures and malformed requests, stays unclassified: a bounded
// retry, then stop.
func statusClass(status int) error {
	switch status {
	case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusTooManyRequests,
		http.StatusNotFound, http.StatusMethodNotAllowed:
		return engine.ErrChildCustodyUnavailable
	case http.StatusConflict:
		return engine.ErrChildCustodyRefused
	default:
		return nil
	}
}

// ObserveChildHandoff reads control-owned acceptance, never pod output.
func (c *Client) ObserveChildHandoff(ctx context.Context, request engine.ChildHandoffObservation) (*journal.ChildHandoffRequest, error) {
	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", engine.ErrChildCustodyRefused, err)
	}
	var out *journal.ChildHandoffRequest
	err := c.call(ctx, apicontract.EngineChildHandoffPath, request, http.StatusOK, maxReceiptBytes, &out)
	return out, err
}
