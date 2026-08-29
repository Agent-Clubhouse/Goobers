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
)

// defaultCredentialTimeout bounds a resolve. Short on purpose: credentials are
// resolved at stage START (DS9/DS10), so a hang here delays every stage rather
// than one late write, and a stage that cannot get its credentials must fail
// fast rather than run without them.
const defaultCredentialTimeout = 30 * time.Second

// MintedCredential mirrors httpapi.MintedCredential. Restated rather than
// imported: internal/httpapi is the SERVER, and a pod-side client that imports
// its server would drag the whole daemon surface into the stage binary.
type MintedCredential struct {
	Capability string `json:"capability"`
	Value      string `json:"value"`
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
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return nil, errors.New("dispatcher: credential client has no base URL")
	}
	if runID == "" || stage == "" {
		return nil, fmt.Errorf("dispatcher: credential resolve requires run and stage (got run %q stage %q)", runID, stage)
	}
	body, err := json.Marshal(struct {
		RunID        string   `json:"runId"`
		Stage        string   `json:"stage"`
		Capabilities []string `json:"capabilities,omitempty"`
	}{RunID: runID, Stage: stage, Capabilities: capabilities})
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
	resp, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("dispatcher: credential resolve to %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("dispatcher: read credential resolve response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// The body may name the refused capability, which is the whole
		// diagnostic — a scoping refusal and a transport fault must not read
		// the same. Truncated so a large error page cannot flood a stage log.
		detail := strings.TrimSpace(string(payload))
		if len(detail) > 400 {
			detail = detail[:400] + "…"
		}
		return nil, &CredentialResolveRefusal{Status: resp.StatusCode, Detail: detail}
	}
	var decoded struct {
		Credentials []MintedCredential `json:"credentials"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("dispatcher: decode credential resolve response: %w", err)
	}
	// A granted capability that resolved to an EMPTY value is a fault, not a
	// silent no-op: the stage would run believing it was credentialed and fail
	// somewhere far away, against the provider.
	for _, cred := range decoded.Credentials {
		if strings.TrimSpace(cred.Value) == "" {
			return nil, fmt.Errorf("dispatcher: credential plane returned an empty value for capability %q", cred.Capability)
		}
	}
	return decoded.Credentials, nil
}
