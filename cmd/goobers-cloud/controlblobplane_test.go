package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sharedauth "github.com/goobers/goobers/internal/podauth"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/cloud/acceptance"
	"github.com/goobers/goobers/internal/cloud/control"
	"github.com/goobers/goobers/internal/cloud/lease"
	cloudauth "github.com/goobers/goobers/internal/cloud/podauth"
	"github.com/goobers/goobers/internal/httpapi"
)

// A control replica that does not hold the lease (a standby) still serves the
// blob plane through its real handler; every other plane stays gated (#8188).
func TestControlStandbyServesTheBlobPlaneOnly(t *testing.T) {
	host, err := control.New(control.Config{ValidateAssembly: func() error { return nil }, SelfExecution: "deny", Leadership: controlTestLeadership{}, SchemaCompatible: func(context.Context) error { return nil }, BuildEpoch: func(context.Context, *lease.Handle) (control.EpochServices, error) {
		return control.EpochServices{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if host.Ready() {
		t.Fatal("premise: the test host must be a standby")
	}
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	planes := populatedControlPlanes()
	planes.BlobPlane = httpapi.WithBlobService(store)
	db := databaseServices{StartStatusPage: func(context.Context, string, int) ([]acceptance.StartStatus, error) { return nil, nil }}
	keyPath := filepath.Join(t.TempDir(), "keys.json")
	raw, err := json.Marshal(map[string]sharedauth.SigningKey{"current": {ID: "test", Key: bytes.Repeat([]byte("k"), 32)}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	auth, err := cloudauth.New(cloudauth.Config{Mode: "signed", KeyFile: keyPath, Revocations: admitAllGrants{}, Fallback: httpapi.DenyAllAuthenticator{}})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := controlHandler(db, planes, auth, host, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	recovering := func(method, path string) bool {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(method, path, bytes.NewReader([]byte("x"))))
		var envelope httpapi.ErrorEnvelope
		_ = json.Unmarshal(response.Body.Bytes(), &envelope)
		return response.Code == http.StatusServiceUnavailable && envelope.Error.Code == httpapi.CodeRecovering
	}
	blobPath := strings.Replace(apicontract.BlobDigestPath, "{digest}", "sha256:"+strings.Repeat("b", 64), 1)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		if recovering(method, blobPath) {
			t.Fatalf("%s blob on a standby control replica was gated", method)
		}
	}
	if !recovering(http.MethodGet, httpapi.RunsPath) {
		t.Fatal("a non-blob route was served on a standby control replica")
	}
}

// The blob plane seam serves control's own object store, and refuses to
// compose without one.
func TestConstructBlobPlaneServesTheObjectStore(t *testing.T) {
	var planes controlPlanes
	if err := constructBlobPlane(t.Context(), databaseServices{}, controlAuth{}, &lease.Manager{}, func() *lease.Handle { return nil }, &planes); err == nil || planes.BlobPlane != nil {
		t.Fatalf("composed without an object store: %v", err)
	}
	store, err := blobstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := constructBlobPlane(t.Context(), databaseServices{Blobs: store}, controlAuth{}, &lease.Manager{}, func() *lease.Handle { return nil }, &planes); err != nil || planes.BlobPlane == nil {
		t.Fatalf("compose with an object store = %v", err)
	}
	for _, seam := range controlAssembly {
		if seam.field == "BlobPlane" && seam.controller {
			t.Fatal("the blob plane must not run on the controller pool: it needs no database")
		}
	}
}
