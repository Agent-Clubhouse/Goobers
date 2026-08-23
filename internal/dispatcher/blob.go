package dispatcher

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

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
)

// BlobPathPrefix is the blob endpoint's digest route root: one blob is
// addressed as <endpoint>/api/v1/blobs/<digest> with the digest's own
// "sha256:<hex>" spelling as the final segment. This is the decision-010
// artifact-plane wire contract: a stage pod materializes and surrenders
// artifacts by digest OVER THE NETWORK — never a shared PVC, never a
// dispatcher-brokered byte path. The serving side may start daemon-fronted
// (§2a: goobers-api) and may move object-store-direct behind the same digest
// contract; this constant is what both ends and the NetworkPolicy peers agree
// on.
const BlobPathPrefix = apicontract.V1Prefix + "/blobs/"

// defaultBlobTimeout bounds one blob transfer when no client is supplied.
const defaultBlobTimeout = 60 * time.Second

// BlobClient is the network blob plane client (decision 010): a
// blobstore.Store implementation that fetches and puts sha256 digests over
// HTTP from the blob endpoint, authenticated with a stage-scoped credential.
// It is what a stage pod's materialize/surrender path plugs into
// workerhost.MaterializeContext / StagingArtifacts in place of a local
// directory — same interface, network transport.
//
// The bearer is the pod's stage-scoped credential (podauth per-run token or a
// credential-plane-minted value) — never a shared instance secret.
type BlobClient struct {
	// BaseURL is the blob endpoint root, e.g. "http://goobers-api.goobers-system:7777".
	BaseURL string
	// Token is the stage-scoped bearer presented as Authorization; empty
	// sends none (the loopback/no-auth posture).
	Token string
	// Client overrides the HTTP client; nil uses a 60s-timeout default.
	Client *http.Client
}

func (c *BlobClient) httpClient() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return &http.Client{Timeout: defaultBlobTimeout}
}

func (c *BlobClient) blobURL(digest string) (string, error) {
	base := strings.TrimRight(c.BaseURL, "/")
	if base == "" {
		return "", errors.New("dispatcher: blob client has no base URL")
	}
	if digest == "" {
		return "", errors.New("dispatcher: blob digest must not be empty")
	}
	return base + BlobPathPrefix + url.PathEscape(digest), nil
}

func (c *BlobClient) do(ctx context.Context, method, digest string, body io.Reader) (*http.Response, error) {
	target, err := c.blobURL(digest)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	if c.Token != "" {
		request.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/octet-stream")
	}
	return c.httpClient().Do(request)
}

// Get fetches the blob's bytes by digest; a 404 maps to
// blobstore.ErrNotFound, keeping the fail-soft materialize contract
// (workerhost.MaterializeContext) intact over the network.
func (c *BlobClient) Get(ctx context.Context, digest string) ([]byte, error) {
	response, err := c.do(ctx, http.MethodGet, digest, nil)
	if err != nil {
		return nil, fmt.Errorf("dispatcher: fetch blob %s: %w", digest, err)
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusOK:
		data, err := io.ReadAll(response.Body)
		if err != nil {
			return nil, fmt.Errorf("dispatcher: read blob %s: %w", digest, err)
		}
		return data, nil
	case http.StatusNotFound:
		return nil, blobstore.ErrNotFound
	default:
		return nil, fmt.Errorf("dispatcher: fetch blob %s: endpoint answered %s", digest, response.Status)
	}
}

// Put stores data under digest. The endpoint's write is idempotent by
// content-address (a present digest is a no-op), which is the property that
// makes the plane safe to share fleet-wide with no locking.
func (c *BlobClient) Put(ctx context.Context, digest string, data []byte) error {
	response, err := c.do(ctx, http.MethodPut, digest, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("dispatcher: put blob %s: %w", digest, err)
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusOK, http.StatusCreated, http.StatusNoContent:
		return nil
	default:
		return fmt.Errorf("dispatcher: put blob %s: endpoint answered %s", digest, response.Status)
	}
}

// Has reports digest presence via HEAD, so a caller can skip a large Get.
func (c *BlobClient) Has(ctx context.Context, digest string) (bool, error) {
	response, err := c.do(ctx, http.MethodHead, digest, nil)
	if err != nil {
		return false, fmt.Errorf("dispatcher: probe blob %s: %w", digest, err)
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("dispatcher: probe blob %s: endpoint answered %s", digest, response.Status)
	}
}

// Describe names the endpoint for diagnostics, credential-free.
func (c *BlobClient) Describe() string {
	return "blob-endpoint:" + strings.TrimRight(c.BaseURL, "/")
}

// StageCredentialRequest asks the daemon credential plane for one stage's
// scoped credentials (decision 010's "stage-scoped credential" +
// distributed-state DS9/DS10). Mirrors the wire shape of the write API's
// resolve route without importing the daemon-side package.
type StageCredentialRequest struct {
	// RunID and Stage identify the requesting stage.
	RunID string `json:"runId"`
	// Stage is the stage name, verified daemon-side against the run's PINNED
	// workflow definition.
	Stage string `json:"stage"`
	// Capabilities optionally narrows resolution to a declared subset.
	Capabilities []string `json:"capabilities,omitempty"`
}

// StageCredential is one minted, stage-scoped credential value.
type StageCredential struct {
	// Capability names the credential capability the value backs.
	Capability string `json:"capability"`
	// Value is the minted secret — a snapshot of a refreshing source, not a
	// lease (DS10).
	Value string `json:"value"`
	// ExpiresAt is the stated expiry when the source states one.
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
}

// stageCredentialResponse mirrors the resolve route's response envelope.
type stageCredentialResponse struct {
	Credentials []StageCredential `json:"credentials"`
}

// ResolveStageCredentials requests stage-scoped credentials from the daemon
// write API's credential plane (POST apicontract.CredentialResolvePath),
// authenticated with the pod's per-run bearer. Consumer contract (DS10):
// resolve at stage start, honor ExpiresAt, one re-resolve-and-retry on 401.
func ResolveStageCredentials(ctx context.Context, client *http.Client, baseURL, podToken string, request StageCredentialRequest) ([]StageCredential, error) {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		return nil, errors.New("dispatcher: credential resolve has no base URL")
	}
	if request.RunID == "" || request.Stage == "" {
		return nil, errors.New("dispatcher: credential resolve requires runId and stage")
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("dispatcher: encode credential resolve request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, base+apicontract.CredentialResolvePath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if podToken != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+podToken)
	}
	if client == nil {
		client = &http.Client{Timeout: defaultBlobTimeout}
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("dispatcher: credential resolve: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return nil, fmt.Errorf("dispatcher: credential resolve for run %s stage %s: plane answered %s: %s",
			request.RunID, request.Stage, response.Status, strings.TrimSpace(string(body)))
	}
	var decoded stageCredentialResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("dispatcher: decode credential resolve response: %w", err)
	}
	return decoded.Credentials, nil
}
