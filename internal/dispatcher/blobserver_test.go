package dispatcher

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
)

// blobserver_test.go proves the round trip decision 010/012 promises: the
// REAL BlobClient (this package's own shipped pod-side client) against the
// REAL daemon blob-plane handler (internal/httpapi), authenticated through
// the REAL podauth bearer path every other pod-facing write-API route uses —
// not a hand-rolled fake server standing in for either side. blob_test.go's
// fakeBlobEndpoint proves the CLIENT's wire shape in isolation; this file
// proves the two ends actually agree.

// stubReadService satisfies readservice.Reader with zero-value responses.
// None of these routes are exercised by the blob-plane tests below; the
// handler just needs a non-nil reader to construct.
type stubReadService struct{}

func (stubReadService) Health(context.Context) (readservice.Health, error) {
	return readservice.Health{}, nil
}
func (stubReadService) PortalConfig(context.Context) (readservice.PortalConfig, error) {
	return readservice.PortalConfig{}, nil
}
func (stubReadService) TelemetryStats(context.Context, readservice.TelemetryStatsRequest) (readservice.TelemetryStatsResult, error) {
	return readservice.TelemetryStatsResult{}, nil
}
func (stubReadService) TelemetryErrorSignatures(context.Context, readservice.TelemetryErrorSignaturesRequest) (readservice.TelemetryErrorSignaturesResult, error) {
	return readservice.TelemetryErrorSignaturesResult{}, nil
}
func (stubReadService) TelemetryErrors(context.Context, readservice.TelemetryErrorsRequest) (readservice.TelemetryErrorsPage, error) {
	return readservice.TelemetryErrorsPage{}, nil
}
func (stubReadService) TelemetryImplementationOutcomes(context.Context, readservice.TelemetryImplementationOutcomesRequest) (readservice.TelemetryImplementationOutcomesResult, error) {
	return readservice.TelemetryImplementationOutcomesResult{}, nil
}
func (stubReadService) ListRuns(context.Context, readservice.RunListOptions) (readservice.RunList, error) {
	return readservice.RunList{}, nil
}
func (stubReadService) GetRun(context.Context, string) (readservice.RunDetail, error) {
	return readservice.RunDetail{}, nil
}
func (stubReadService) RunEvents(context.Context, string) (readservice.EventList, error) {
	return readservice.EventList{}, nil
}
func (stubReadService) StageAttempts(context.Context, string, string) (readservice.AttemptList, error) {
	return readservice.AttemptList{}, nil
}
func (stubReadService) Artifact(context.Context, string, string) (readservice.ArtifactContent, error) {
	return readservice.ArtifactContent{}, nil
}
func (stubReadService) Transcript(context.Context, string, uint64) (readservice.TranscriptContent, error) {
	return readservice.TranscriptContent{}, nil
}
func (stubReadService) Instance(context.Context) (readservice.Instance, error) {
	return readservice.Instance{}, nil
}
func (stubReadService) Gaggles(context.Context, readservice.PageRequest) (readservice.GagglePage, error) {
	return readservice.GagglePage{}, nil
}
func (stubReadService) Goobers(context.Context, string, readservice.PageRequest) (readservice.GooberPage, error) {
	return readservice.GooberPage{}, nil
}
func (stubReadService) Workflows(context.Context, string, readservice.PageRequest) (readservice.WorkflowPage, error) {
	return readservice.WorkflowPage{}, nil
}
func (stubReadService) Connections(context.Context, string) (readservice.GaggleConnections, error) {
	return readservice.GaggleConnections{}, nil
}
func (stubReadService) Workflow(context.Context, string, string) (readservice.WorkflowDetail, error) {
	return readservice.WorkflowDetail{}, nil
}

// humanFallbackAuthenticator stands in for oidcauth: any bearer prefixed
// "human." authenticates as a human principal holding every role, so a test
// can prove a human is refused by the blob plane even though it clears the
// router-level role floor — the same double-layered gate the credential
// plane already uses (DS9).
type humanFallbackAuthenticator struct{}

func (humanFallbackAuthenticator) Authenticate(request *http.Request) (*httpapi.Principal, error) {
	authorization := request.Header.Get("Authorization")
	const prefix = "Bearer human."
	if !strings.HasPrefix(authorization, prefix) {
		return nil, errors.New("no human bearer presented")
	}
	return &httpapi.Principal{
		Subject: "human:" + strings.TrimPrefix(authorization, prefix),
		Roles:   []httpapi.Role{httpapi.RoleAdmin},
	}, nil
}

// blobServerFixture wires a handler shaped exactly like the daemon's own
// construction in cmd/goobers/up.go: podauth chained in front of the human
// fallback, httpapi.RequireRoles() as the authorizer, a real directory-backed
// blobstore.Store behind httpapi.WithBlobService.
type blobServerFixture struct {
	server   *httptest.Server
	registry *podauth.Registry
}

func newBlobServerFixture(t *testing.T) *blobServerFixture {
	t.Helper()
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("blobstore.NewDir: %v", err)
	}
	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, humanFallbackAuthenticator{})
	if err != nil {
		t.Fatalf("podauth.NewAuthenticator: %v", err)
	}
	handler, err := httpapi.NewHandler(stubReadService{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator),
		httpapi.WithBlobService(store),
	)
	if err != nil {
		t.Fatalf("httpapi.NewHandler: %v", err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &blobServerFixture{server: server, registry: registry}
}

// client returns a real BlobClient authenticated as runID's stage pod — the
// exact type mode-3 stage pods construct in production.
func (f *blobServerFixture) client(t *testing.T, runID string) *BlobClient {
	t.Helper()
	token, err := f.registry.Mint(runID, 0)
	if err != nil {
		t.Fatalf("mint pod token: %v", err)
	}
	return &BlobClient{BaseURL: f.server.URL, Token: token}
}

// TestBlobServerRoundTrip is the client contract's central promise: a stage
// pod's BlobClient.Put followed by its own BlobClient.Get returns the exact
// bytes, over the network, through the real daemon handler.
func TestBlobServerRoundTrip(t *testing.T) {
	fixture := newBlobServerFixture(t)
	client := fixture.client(t, "run-1")

	data := []byte("mode-3 artifact bytes")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	ctx := context.Background()

	// Missing before Put: the fail-soft materialize contract (blobstore.ErrNotFound).
	if _, err := client.Get(ctx, digest); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("Get before Put = %v, want blobstore.ErrNotFound", err)
	}

	if err := client.Put(ctx, digest, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := client.Get(ctx, digest)
	if err != nil {
		t.Fatalf("Get after Put: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("round trip mismatch: got %q, want %q", got, data)
	}

	// Idempotent: a retried Put of the same (digest-verified, hence identical)
	// content succeeds again rather than conflicting.
	if err := client.Put(ctx, digest, data); err != nil {
		t.Fatalf("repeated Put: %v", err)
	}

	// A second run's pod can read what the first run's pod stored — the store
	// is fleet-wide and content-addressed, not per-run.
	other := fixture.client(t, "run-2")
	got, err = other.Get(ctx, digest)
	if err != nil || string(got) != string(data) {
		t.Fatalf("cross-run Get = %q, %v", got, err)
	}
}

// TestBlobServerRejectsDigestMismatch proves the integrity check the whole
// plane exists for: a PUT whose body does not hash to the digest named in the
// URL is refused, never silently filed under the wrong address.
func TestBlobServerRejectsDigestMismatch(t *testing.T) {
	fixture := newBlobServerFixture(t)
	client := fixture.client(t, "run-1")

	data := []byte("real bytes")
	wrongDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("different bytes")))

	err := client.Put(context.Background(), wrongDigest, data)
	if err == nil {
		t.Fatal("Put with a mismatched digest succeeded")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("Put mismatch error = %v, want a 400", err)
	}

	// Refused before it ever reached the store: a subsequent Get for the
	// wrong digest still 404s.
	if _, err := client.Get(context.Background(), wrongDigest); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("Get of a refused digest = %v, want blobstore.ErrNotFound", err)
	}
}

// TestBlobServerGetMissingIs404 pins the fail-soft contract for an absent
// digest: a well-formed digest nobody has ever Put maps to blobstore.ErrNotFound.
func TestBlobServerGetMissingIs404(t *testing.T) {
	fixture := newBlobServerFixture(t)
	client := fixture.client(t, "run-1")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("never stored")))
	if _, err := client.Get(context.Background(), digest); !errors.Is(err, blobstore.ErrNotFound) {
		t.Fatalf("Get of a never-stored digest = %v, want blobstore.ErrNotFound", err)
	}
}

// TestBlobServerRejectsInvalidDigestFormat proves a malformed digest fails
// closed with a 400 before ever reaching the store, rather than surfacing as
// an opaque 500 or being silently accepted.
func TestBlobServerRejectsInvalidDigestFormat(t *testing.T) {
	fixture := newBlobServerFixture(t)
	client := fixture.client(t, "run-1")

	if _, err := client.Get(context.Background(), "not-a-digest"); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("Get with a malformed digest = %v, want a 400", err)
	}
	if err := client.Put(context.Background(), "sha256:tooshort", []byte("x")); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("Put with a malformed digest = %v, want a 400", err)
	}
}

// TestBlobServerRequiresAuthentication proves the auth rejection cases: no
// bearer at all, an unminted/wrong pod token, and a fully authenticated but
// non-pod (human) principal are all refused — the blob plane serves stage
// pods only, mirroring the credential plane's unconditional pod-principal
// gate (DS9), never a hand-rolled check.
func TestBlobServerRequiresAuthentication(t *testing.T) {
	fixture := newBlobServerFixture(t)
	data := []byte("secret-ish bytes")
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	ctx := context.Background()

	// No bearer at all: RequireRoles refuses an unauthenticated principal
	// outright (401) — the human fallback declines it, and podauth never sees
	// a pod-prefixed token to verify.
	anonymous := &BlobClient{BaseURL: fixture.server.URL}
	if err := anonymous.Put(ctx, digest, data); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthenticated Put = %v, want a 401", err)
	}
	if _, err := anonymous.Get(ctx, digest); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unauthenticated Get = %v, want a 401", err)
	}

	// A pod-shaped bearer the registry never minted: podauth fails it closed
	// as unknown rather than falling through to the human fallback (401).
	wrongToken := &BlobClient{BaseURL: fixture.server.URL, Token: "goobers-pod.never-minted"}
	if err := wrongToken.Put(ctx, digest, data); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unminted pod token Put = %v, want a 401", err)
	}

	// A fully authenticated human principal, holding every role, still gets
	// refused: the blob plane's digest carries no run to scope a check
	// against, so containment is pod-principal-or-nothing, not role rank.
	humanReq, err := http.NewRequest(http.MethodGet, fixture.server.URL+BlobPathPrefix+digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	humanReq.Header.Set("Authorization", "Bearer human.admin")
	response, err := fixture.server.Client().Do(humanReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("human principal GET status = %d, want 403", response.StatusCode)
	}
}
