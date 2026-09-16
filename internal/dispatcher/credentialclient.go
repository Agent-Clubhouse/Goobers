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

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/workspacerevision"
)

// defaultCredentialTimeout bounds a resolve. Short on purpose: credentials are
// resolved at stage START (DS9/DS10), so a hang here delays every stage rather
// than one late write, and a stage that cannot get its credentials must fail
// fast rather than run without them.
const defaultCredentialTimeout = 30 * time.Second

// WorkspaceRevisionCheckoutCapability identifies a checkout-only source grant,
// never a stage-declarable capability or a base-repository credential.
const WorkspaceRevisionCheckoutCapability = "workspace-revision:checkout"

// WorkspaceBranchCheckoutCapability is consumed only by owned pod provisioning.
const WorkspaceBranchCheckoutCapability = "workspace-branch:checkout"

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
	// Anonymous is an internal attestation of an empty successful selected-
	// checkout response, never an externally supplied credential property.
	Anonymous bool `json:"-"`
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
	// RetryDeadline bounds how long Resolve retries a transport error or 5xx
	// response before giving up. Zero uses defaultCredentialRetryDeadline.
	RetryDeadline time.Duration
}

// CredentialResolveRefusal is the credential plane's own answer to a resolve —
// a non-200 status carrying the plane's diagnostic — as distinct from a
// transport fault (a dial that never reached the plane, a timeout, an
// unreadable body), which Resolve returns untyped. The split is what a pod
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

// Resolve returns the credentials the daemon grants this run's stage. An empty
// capability list resolves to nothing WITHOUT calling the daemon: a stage that
// declared no capabilities must not cause a credential request at all.
//
// A non-200 answer from the plane is returned as a *CredentialResolveRefusal;
// every other failure — including a plane that could not be reached — is an
// untyped error, so errors.As on the refusal type separates the plane's
// judgement from the transport's.
func (c *CredentialResolveClient) Resolve(ctx context.Context, runID, stage string, capabilities []string) ([]MintedCredential, error) {
	if len(capabilities) == 0 {
		return nil, nil
	}
	return c.resolve(ctx, runID, stage, capabilities, nil, nil)
}

// ResolveCheckout asks the daemon to reauthorize the selected source using the
// run's configured repositories and existing credentials. No URL or selector
// reconstructed from the result is ever sent as credential authority.
func (c *CredentialResolveClient) ResolveCheckout(ctx context.Context, runID, stage string, revision *apiv1.WorkspaceRevision) ([]MintedCredential, error) {
	if revision == nil {
		return nil, &workspacerevision.Error{Code: workspacerevision.CodeInvalid, Message: "selected revision checkout requires identity"}
	}
	creds, err := c.resolve(ctx, runID, stage, nil, revision, nil)
	if err == nil {
		if creds == nil {
			return nil, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized,
				Message: "selected checkout response omitted its credential authorization"}
		}
		if len(creds) == 0 {
			return []MintedCredential{{Capability: WorkspaceRevisionCheckoutCapability, Anonymous: true}}, nil
		}
		if len(creds) != 1 || creds[0].Capability != WorkspaceRevisionCheckoutCapability {
			return nil, &workspacerevision.Error{Code: workspacerevision.CodeUnauthorized,
				Message: "selected checkout response must contain only one source checkout credential"}
		}
		return creds, nil
	}
	code := workspacerevision.CodeAcquisition
	var refusal *CredentialResolveRefusal
	if errors.As(err, &refusal) {
		if refusal.Deterministic() {
			code = workspacerevision.CodeUnauthorized
		}
		var detail apicontract.APIError
		if json.Unmarshal([]byte(refusal.Detail), &detail) == nil {
			switch detail.Code {
			case workspacerevision.CodeInvalid, workspacerevision.CodeUnauthorized, workspacerevision.CodeConflict,
				workspacerevision.CodeAcquisition, workspacerevision.CodeObjectType, workspacerevision.CodeSHAMismatch:
				code = detail.Code
			}
		}
	}
	return nil, &workspacerevision.Error{Code: code, Message: "selected source credential could not be resolved", Cause: err}
}

func (c *CredentialResolveClient) resolve(ctx context.Context, runID, stage string, capabilities []string, revision *apiv1.WorkspaceRevision, binding *apiv1.WorkspaceBranchBinding) ([]MintedCredential, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return nil, errors.New("dispatcher: credential client has no base URL")
	}
	if runID == "" || stage == "" {
		return nil, fmt.Errorf("dispatcher: credential resolve requires run and stage (got run %q stage %q)", runID, stage)
	}
	body, err := json.Marshal(struct {
		RunID                  string                        `json:"runId"`
		Stage                  string                        `json:"stage"`
		Capabilities           []string                      `json:"capabilities,omitempty"`
		WorkspaceRevision      *apiv1.WorkspaceRevision      `json:"workspaceRevision,omitempty"`
		WorkspaceBranchBinding *apiv1.WorkspaceBranchBinding `json:"workspaceBranchBinding,omitempty"`
	}{RunID: runID, Stage: stage, Capabilities: capabilities, WorkspaceRevision: revision, WorkspaceBranchBinding: binding})
	if err != nil {
		return nil, fmt.Errorf("dispatcher: encode credential resolve request: %w", err)
	}
	endpoint := base + apicontract.CredentialResolvePath
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("dispatcher: build credential resolve request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: defaultCredentialTimeout}
	}
	deadline := c.RetryDeadline
	if deadline <= 0 {
		deadline = defaultCredentialRetryDeadline
	}

	// Retrying a resolve is safe by construction: it is a READ of what this
	// run's stage is entitled to, and the plane mints a fresh short-lived
	// credential per call rather than consuming a one-shot grant. A repeated
	// resolve can only return the same entitlement again.
	var credentials []MintedCredential
	retryErr := withRetry(ctx, deadline, func(ctx context.Context) (bool, error) {
		// A fresh request per attempt: an *http.Request body is consumed by
		// the first send, so a retried request would post an empty body and
		// be refused as invalid — a self-inflicted non-retryable failure.
		attempt := request.Clone(ctx)
		attempt.Body = io.NopCloser(bytes.NewReader(body))
		resolved, retryable, err := c.resolveOnce(client, attempt, endpoint)
		if err != nil {
			return retryable, err
		}
		credentials = resolved
		return false, nil
	})
	if retryErr != nil {
		return nil, retryErr
	}
	return credentials, nil
}

// resolveOnce performs one resolve attempt and classifies its failure.
//
// The classification is the plane's existing one, not a new vocabulary: a
// refusal the plane will repeat for every pod of this stage is a configuration
// outcome and must fail fast, while a plane that could not answer (5xx,
// including 503 credentials_unavailable) or could not be reached at all is the
// control-plane restart this retry exists to ride out.
func (c CredentialResolveClient) resolveOnce(client *http.Client, request *http.Request, endpoint string) ([]MintedCredential, bool, error) {
	resp, err := client.Do(request)
	if err != nil {
		// A transport fault never reached the plane: a refused dial or a
		// dropped connection is exactly what a restarting daemon looks like.
		return nil, true, fmt.Errorf("dispatcher: credential resolve to %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, true, fmt.Errorf("dispatcher: read credential resolve response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body may name the refused capability, which is the whole
		// diagnostic — a scoping refusal and a transport fault must not read
		// the same. Truncated so a large error page cannot flood a stage log.
		detail := strings.TrimSpace(string(payload))
		if len(detail) > 400 {
			detail = detail[:400] + "…"
		}
		return nil, retryableStatus(resp.StatusCode), &CredentialResolveRefusal{Status: resp.StatusCode, Detail: detail}
	}
	var decoded struct {
		Credentials []MintedCredential `json:"credentials"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, false, fmt.Errorf("dispatcher: decode credential resolve response: %w", err)
	}
	// A granted capability that resolved to an EMPTY value is a fault, not a
	// silent no-op: the stage would run believing it was credentialed and fail
	// somewhere far away, against the provider.
	for _, cred := range decoded.Credentials {
		if strings.TrimSpace(cred.Value) == "" {
			return nil, false, fmt.Errorf("dispatcher: credential plane returned an empty value for capability %q", cred.Capability)
		}
	}
	return decoded.Credentials, false, nil
}
