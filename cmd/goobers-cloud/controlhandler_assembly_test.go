package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudauth "github.com/goobers/goobers/internal/cloud/podauth"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journalclient"
	sharedauth "github.com/goobers/goobers/internal/podauth"
)

// admitAllGrants admits every grant: this test is not about revocation.
type admitAllGrants struct{}

func (admitAllGrants) CheckGrant(context.Context, string, time.Time) error { return nil }
func (admitAllGrants) Revoke(context.Context, string, time.Time) error     { return nil }
func (admitAllGrants) IsRevoked(context.Context, string) (bool, error)     { return false, nil }

// phaseRunJournal answers RunPhase so a served request is observable.
type phaseRunJournal struct{ httpapi.RunJournalService }

func (phaseRunJournal) RunPhase(_ context.Context, request journalclient.RunPhaseRequest) (journalclient.RunPhaseResponse, error) {
	return journalclient.RunPhaseResponse{RunID: request.TargetRunID, Phase: "completed"}, nil
}

// TestControlHandlerServesTheFullAssembly: with every seam composed the
// assembly is complete and controlHandler builds the full API. On a standby
// (the recovery gate closed) recovery-safe routes serve and gated routes
// answer "recovering"; once the host is ready they serve. Leaving any one seam
// unassigned leaves only the status handler, so the API route is not served.
func TestControlHandlerServesTheFullAssembly(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "keys.json")
	raw, err := json.Marshal(map[string]sharedauth.SigningKey{"current": {ID: "test", Key: bytes.Repeat([]byte("k"), 32)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := sharedauth.LoadKeyRingFile(keyPath, "")
	if err != nil {
		t.Fatal(err)
	}
	auth, err := cloudauth.New(cloudauth.Config{Mode: "signed", KeyFile: keyPath, Revocations: admitAllGrants{}, Fallback: httpapi.DenyAllAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	runID := strings.Repeat("a", 32)
	token, err := keys.MintScoped(runID, time.Hour, httpapi.ScopeJournal)
	if err != nil {
		t.Fatal(err)
	}
	full := populatedControlPlanes()
	full.RunJournal = phaseRunJournal{}
	// The fixture fills the option-valued seams with a deny-all authenticator,
	// which would replace the real one; here each re-installs the real one.
	for _, option := range []*httpapi.HandlerOption{&full.EngineChildAdmissions, &full.EngineChildAuthority, &full.EngineChildProtocol, &full.ChildWorkflows} {
		*option = httpapi.WithAuthenticator(auth)
	}
	// The blob plane is a real one over a store, so the test can observe it
	// served on a standby as well as once ready (#8188).
	blobs, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blob := []byte("a stage artifact")
	blobSum := sha256.Sum256(blob)
	blobDigest := "sha256:" + hex.EncodeToString(blobSum[:])
	if err := blobs.Put(t.Context(), blobDigest, blob); err != nil {
		t.Fatal(err)
	}
	full.BlobPlane = httpapi.WithBlobService(blobs)
	blobBearer, err := keys.MintWorkerBlob("goobers-worker-0", 2*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !full.complete() {
		t.Fatalf("a fully composed assembly reports missing seams: %v", full.missing())
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	release := make(chan struct{})
	defer close(release)
	ok := func(context.Context) error { return nil }
	host, err := control.New(control.Config{ValidateAssembly: func() error { return nil }, SelfExecution: "deny", Leadership: activeControlTestLeadership{}, SchemaCompatible: ok, BuildEpoch: func(context.Context, *lease.Handle) (control.EpochServices, error) {
		return control.EpochServices{RebuildBeforeReap: ok, ReconcileUncertain: ok, ReattachAwaiters: ok, Stop: ok, L2: ok, L3: ok, L1: func(ctx context.Context) error {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		}}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	db := databaseServices{StartStatusPage: func(context.Context, string, int) ([]acceptance.StartStatus, error) { return nil, nil }}
	build := func(p controlPlanes) http.Handler {
		t.Helper()
		handler, err := controlHandler(db, p, auth, host, log.New(io.Discard, "", 0))
		if err != nil {
			t.Fatal(err)
		}
		return handler
	}
	runPhase := func(handler http.Handler) *httptest.ResponseRecorder {
		body, err := json.Marshal(journalclient.RunPhaseRequest{RunID: runID, Gaggle: "web", TargetRunID: strings.Repeat("b", 32)})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, apicontract.JournalRunPhasePath, bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	// The blob plane is lease-free: a worker reads by digest on a standby and
	// once ready alike.
	blobGet := func(handler http.Handler) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, strings.Replace(apicontract.BlobDigestPath, "{digest}", blobDigest, 1), nil)
		request.Header.Set("Authorization", "Bearer "+blobBearer)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	// discovery is recovery-safe: it passes the gate even on a standby (a pod
	// principal is then refused by authentication, which is past the gate).
	discovery := func(handler http.Handler) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.DiscoveryPath, nil))
		return response
	}

	// Standby: the host has not taken the lease, so the gate is closed.
	handler := build(full)
	if host.Ready() {
		t.Fatal("precondition: the host must start as a standby")
	}
	if response := discovery(handler); response.Code == http.StatusServiceUnavailable || strings.Contains(response.Body.String(), httpapi.CodeRecovering) {
		t.Fatalf("standby discovery = %d %s, want a recovery-safe route past the gate", response.Code, response.Body.String())
	}
	if response := runPhase(handler); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), httpapi.CodeRecovering) {
		t.Fatalf("standby run phase = %d %s, want 503 %s", response.Code, response.Body.String(), httpapi.CodeRecovering)
	}
	if response := blobGet(handler); response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), blob) {
		t.Fatalf("standby blob GET = %d %s, want the blob served lease-free", response.Code, response.Body.String())
	}

	// Ready: the host holds the lease and has finished its startup sequence.
	go func() { _ = host.Run(ctx) }()
	deadline := time.Now().Add(10 * time.Second)
	for !host.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("the host never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if response := runPhase(handler); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "completed") {
		t.Fatalf("ready run phase = %d %s, want it served", response.Code, response.Body.String())
	}
	if response := blobGet(handler); response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), blob) {
		t.Fatalf("ready blob GET = %d %s, want the blob served", response.Code, response.Body.String())
	}

	// Mutants: any one seam left unassigned must not serve the API.
	for _, seam := range controlAssembly {
		partial := full
		field := reflect.ValueOf(&partial).Elem().FieldByName(seam.field)
		if !field.IsValid() {
			t.Fatalf("assembly seam %s has no field on controlPlanes", seam.field)
		}
		field.Set(reflect.Zero(field.Type()))
		if partial.complete() {
			t.Errorf("without %s the assembly still reports complete", seam.field)
			continue
		}
		if response := runPhase(build(partial)); response.Code == http.StatusOK {
			t.Errorf("without %s the API was served", seam.field)
		}
	}
}
