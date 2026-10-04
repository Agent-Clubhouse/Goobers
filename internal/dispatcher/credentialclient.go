package dispatcher

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
	"github.com/goobers/goobers/internal/daemonclient"
)

// defaultCredentialTimeout bounds a resolve. Short on purpose: credentials are
// resolved at stage START (DS9/DS10), so a hang here delays every stage rather
// than one late write, and a stage that cannot get its credentials must fail
// fast rather than run without them.
const defaultCredentialTimeout = 30 * time.Second

// defaultCredentialRetryDeadline bounds the WHOLE resolve loop — across
// attempts — when the caller sets no RetryDeadline of its own (#3809).
//
// The daemon is single-replica by construction: its journal, intake DB, read
// model and telemetry DB live on a ReadWriteOnce volume, so a second replica
// cannot mount it. Every upgrade is therefore stop-then-start, and startup
// resumes interrupted runs — roughly two minutes on a busy instance, and
// growing with run volume. A restart is a ROUTINE event, and a routine event
// should not fail in-flight work.
//
// Before this, a stage whose credential resolve landed in that window failed
// outright: the plane's own doc notes recovery came from spending a FRESH POD,
// which is a whole dispatch cycle to survive something that lasts seconds to
// minutes.
//
// Three minutes covers the observed restart window with margin while staying
// well inside a stage's own timeout. It does not slow the failure that
// matters: a refusal the plane will repeat (403 capability_undeclared, 409
// gate_pin_missing, 400 invalid_request) is classified non-retryable and still
// fails immediately.
const defaultCredentialRetryDeadline = 3 * time.Minute

// MintedCredential mirrors httpapi.MintedCredential. Restated rather than
// imported: internal/httpapi is the SERVER, and a pod-side client that imports
// its server would drag the whole daemon surface into the stage binary.
type MintedCredential struct {
	Capability string `json:"capability"`
	Value      string `json:"value"`
	// ExpiresAt is the expiry the plane stated for Value
	// (httpapi.MintedCredential), nil when its source states none. The pod
	// delivers it to the stage beside the value (#5905).
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// CredentialResolution is one resolve answer: the minted credentials, plus
// the non-secret authorization scheme ("basic" or "bearer") the plane states
// for an Azure DevOps repository credential (httpapi.CredentialResolveResponse
// RepoAuthScheme). RepoAuthScheme is empty for every other provider.
//
// Grant is the stage credential-refresh grant the plane minted when the
// request asked for one and the stage qualifies (Goobers#6120); nil otherwise.
type CredentialResolution struct {
	Credentials    []MintedCredential
	RepoAuthScheme string
	Grant          *CredentialGrant
}

// CredentialGrant mirrors httpapi.CredentialGrantDelivery.
type CredentialGrant struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// CredentialResolveClient resolves a stage's declared credential capabilities
// against the daemon's credential plane (distributed-state-and-coordination.md
// §11). The plane exists FOR stage pods: a pod authenticated as its run
// receives short-lived credentials scoped to exactly the capabilities its
// stage declared — resolution happens at stage start and is never inherited
// from dispatch time, so a dispatch payload never carries a secret.
type CredentialResolveClient struct {
	// BaseURL is the daemon API root (GOOBERS_DAEMON_API in the pod).
	BaseURL string
	// Token is the per-run bearer (GOOBERS_POD_TOKEN in the pod).
	Token string
	// Client overrides the HTTP client; nil uses a bounded default.
	Client *http.Client
	// RetryDeadline bounds how long ResolveStage retries a transport error or 5xx
	// response before giving up. Zero uses defaultCredentialRetryDeadline.
	RetryDeadline time.Duration
	// RetryPolicy overrides retry pacing for this client. Zero values retain
	// the production defaults.
	RetryPolicy RetryPolicy
}

// CredentialResolveRefusal is the credential plane's own answer to a resolve —
// a non-200 status carrying the plane's diagnostic — as distinct from a
// transport fault (a dial that never reached the plane, a timeout, an
// unreadable body), which ResolveStage returns untyped. The split is what a pod
// classifies a failed resolve by: a refusal the plane will repeat for every
// pod of this stage (403 capability_undeclared, 409 gate_pin_missing, 400
// invalid_request) is a configuration outcome, and spending a fresh pod on it
// reproduces it; a plane that could not answer (503 credentials_unavailable,
// any 5xx) may well answer the next pod.
type CredentialResolveRefusal struct {
	// Status is the HTTP status the plane answered with.
	Status int
	// Detail is the plane's (truncated) body — typically the JSON error
	// naming the refused capability or the missing gate pin.
	Detail string
}

func (e *CredentialResolveRefusal) Error() string {
	return fmt.Sprintf("dispatcher: credential resolve refused (%d): %s", e.Status, e.Detail)
}

// Deterministic reports whether the plane would answer the same request the
// same way again: a 4xx is the plane's judgement on the request itself —
// the capability, the run, the pin — and a fresh pod sends the same request.
// The two 4xx codes that by definition ask the client to try again (408
// Request Timeout, 429 Too Many Requests) are the plane's state, not its
// judgement, and stay transport-shaped.
func (e *CredentialResolveRefusal) Deterministic() bool {
	if e.Status == http.StatusRequestTimeout || e.Status == http.StatusTooManyRequests {
		return false
	}
	return e.Status >= 400 && e.Status < 500
}

// ResolveStage returns the credentials the daemon grants this run's stage,
// with the authorization scheme the plane states for an Azure DevOps
// repository credential. An empty capability list resolves to nothing WITHOUT
// calling the daemon: a stage that declared no capabilities must not cause a
// credential request at all.
//
// A non-200 answer from the plane is returned as a *CredentialResolveRefusal;
// every other failure — including a plane that could not be reached — is an
// untyped error, so errors.As on the refusal type separates the plane's
// judgement from the transport's.
func (c *CredentialResolveClient) ResolveStage(ctx context.Context, runID, stage string, capabilities []string) (CredentialResolution, error) {
	return c.Resolve(ctx, CredentialResolveRequest{RunID: runID, Stage: stage, Capabilities: capabilities})
}

// CredentialResolveRequest is one resolve call. Grant asks the plane for a
// stage credential-refresh grant (Goobers#6120) for Attempt, sized from
// TimeoutSeconds when the pinned definition declares no timeout; only the
// pod's stage-start resolve of a deterministic stage sets it.
type CredentialResolveRequest struct {
	RunID          string   `json:"runId"`
	Stage          string   `json:"stage"`
	Capabilities   []string `json:"capabilities,omitempty"`
	Grant          bool     `json:"grant,omitempty"`
	Attempt        int32    `json:"attempt,omitempty"`
	TimeoutSeconds int64    `json:"timeoutSeconds,omitempty"`
}

// Resolve is ResolveStage for a full request.
func (c *CredentialResolveClient) Resolve(ctx context.Context, resolve CredentialResolveRequest) (CredentialResolution, error) {
	if len(resolve.Capabilities) == 0 {
		return CredentialResolution{}, nil
	}
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return CredentialResolution{}, errors.New("dispatcher: credential client has no base URL")
	}
	if resolve.RunID == "" || resolve.Stage == "" {
		return CredentialResolution{}, fmt.Errorf("dispatcher: credential resolve requires run and stage (got run %q stage %q)", resolve.RunID, resolve.Stage)
	}
	body, err := json.Marshal(resolve)
	if err != nil {
		return CredentialResolution{}, fmt.Errorf("dispatcher: encode credential resolve request: %w", err)
	}
	return c.post(ctx, base+apicontract.CredentialResolvePath, body)
}

// post sends one credential-plane request with the client's bearer, retrying
// a transport fault or 5xx until the retry deadline.
func (c *CredentialResolveClient) post(ctx context.Context, endpoint string, body []byte) (CredentialResolution, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return CredentialResolution{}, fmt.Errorf("dispatcher: build credential resolve request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.Client
	if client == nil {
		client = daemonclient.NewHTTP(defaultCredentialTimeout)
	}
	deadline := c.RetryDeadline
	if deadline <= 0 {
		deadline = defaultCredentialRetryDeadline
	}

	// Retrying a resolve is safe by construction: it is a READ of what this
	// run's stage is entitled to, and the plane mints a fresh short-lived
	// credential per call rather than consuming a one-shot grant. A repeated
	// resolve can only return the same entitlement again.
	var resolution CredentialResolution
	retryErr := withRetryPolicy(ctx, deadline, c.RetryPolicy, func(ctx context.Context) (bool, error) {
		// A fresh request per attempt: an *http.Request body is consumed by
		// the first send, so a retried request would post an empty body and
		// be refused as invalid — a self-inflicted non-retryable failure.
		attempt := request.Clone(ctx)
		attempt.Body = io.NopCloser(bytes.NewReader(body))
		resolved, retryable, err := c.resolveOnce(client, attempt, endpoint)
		if err != nil {
			return retryable, err
		}
		resolution = resolved
		return false, nil
	})
	if retryErr != nil {
		return CredentialResolution{}, retryErr
	}
	return resolution, nil
}

// resolveOnce performs one resolve attempt and classifies its failure.
//
// The classification is the plane's existing one, not a new vocabulary: a
// refusal the plane will repeat for every pod of this stage is a configuration
// outcome and must fail fast, while a plane that could not answer (5xx,
// including 503 credentials_unavailable) or could not be reached at all is the
// control-plane restart this retry exists to ride out.
func (c CredentialResolveClient) resolveOnce(client *http.Client, request *http.Request, endpoint string) (CredentialResolution, bool, error) {
	resp, err := client.Do(request)
	if err != nil {
		// A transport fault never reached the plane: a refused dial or a
		// dropped connection is exactly what a restarting daemon looks like.
		return CredentialResolution{}, true, fmt.Errorf("dispatcher: credential resolve to %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return CredentialResolution{}, true, fmt.Errorf("dispatcher: read credential resolve response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body may name the refused capability, which is the whole
		// diagnostic — a scoping refusal and a transport fault must not read
		// the same. Truncated so a large error page cannot flood a stage log.
		detail := strings.TrimSpace(string(payload))
		if len(detail) > 400 {
			detail = detail[:400] + "…"
		}
		return CredentialResolution{}, retryableStatus(resp.StatusCode), &CredentialResolveRefusal{Status: resp.StatusCode, Detail: detail}
	}
	var decoded struct {
		Credentials    []MintedCredential `json:"credentials"`
		RepoAuthScheme string             `json:"repoAuthScheme"`
		Grant          *CredentialGrant   `json:"grant"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return CredentialResolution{}, false, fmt.Errorf("dispatcher: decode credential resolve response: %w", err)
	}
	// A granted capability that resolved to an EMPTY value is a fault, not a
	// silent no-op: the stage would run believing it was credentialed and fail
	// somewhere far away, against the provider.
	for _, cred := range decoded.Credentials {
		if strings.TrimSpace(cred.Value) == "" {
			return CredentialResolution{}, false, fmt.Errorf("dispatcher: credential plane returned an empty value for capability %q", cred.Capability)
		}
	}
	return CredentialResolution{Credentials: decoded.Credentials, RepoAuthScheme: decoded.RepoAuthScheme, Grant: decoded.Grant}, false, nil
}

// defaultCredentialRefreshDeadline bounds a mid-stage refresh's retries. It is
// short: the caller is a live provider request that already has a value to
// fall back on (a proactive refresh) or has already failed (a 401).
const defaultCredentialRefreshDeadline = 30 * time.Second

// CredentialRefreshClient re-resolves one capability mid-stage through the
// credential plane's refresh route, authenticated by the stage's
// credential-refresh grant (Goobers#6120). It is what a deterministic stage —
// local or in a pod — holds instead of the pod token, which it never sees.
type CredentialRefreshClient struct {
	// BaseURL is GOOBERS_CREDENTIAL_ENDPOINT.
	BaseURL string
	// Grant is GOOBERS_CREDENTIAL_GRANT.
	Grant string
	// Client overrides the HTTP client; nil uses a bounded default.
	Client *http.Client
	// RetryDeadline bounds retries; zero uses defaultCredentialRefreshDeadline.
	RetryDeadline time.Duration
	// RetryPolicy overrides retry pacing.
	RetryPolicy RetryPolicy
}

// Refresh returns a freshly minted value for capability. A non-200 answer is a
// *CredentialResolveRefusal, as for ResolveStage.
func (c *CredentialRefreshClient) Refresh(ctx context.Context, capability string) (MintedCredential, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" || c.Grant == "" {
		return MintedCredential{}, errors.New("dispatcher: credential refresh client has no endpoint or grant")
	}
	body, err := json.Marshal(struct {
		Capability string `json:"capability"`
	}{Capability: capability})
	if err != nil {
		return MintedCredential{}, fmt.Errorf("dispatcher: encode credential refresh request: %w", err)
	}
	deadline := c.RetryDeadline
	if deadline <= 0 {
		deadline = defaultCredentialRefreshDeadline
	}
	resolver := CredentialResolveClient{BaseURL: base, Token: c.Grant, Client: c.Client, RetryDeadline: deadline, RetryPolicy: c.RetryPolicy}
	resolution, err := resolver.post(ctx, base+apicontract.CredentialRefreshPath, body)
	if err != nil {
		return MintedCredential{}, err
	}
	for _, minted := range resolution.Credentials {
		if minted.Capability == capability {
			return minted, nil
		}
	}
	return MintedCredential{}, fmt.Errorf("dispatcher: credential refresh returned no value for capability %q", capability)
}
