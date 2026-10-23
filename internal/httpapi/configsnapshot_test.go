package httpapi

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

type configSnapshotFixture struct {
	digest string
	data   []byte
}

func (f configSnapshotFixture) Current() apicontract.ConfigSnapshotCurrent {
	return apicontract.ConfigSnapshotCurrent{Digest: f.digest, InstanceID: "0123456789abcdef0123456789abcdef"}
}

func (f configSnapshotFixture) Open(digest string) (io.ReadCloser, int64, error) {
	if digest != f.digest {
		return nil, 0, io.EOF
	}
	return io.NopCloser(bytes.NewReader(f.data)), int64(len(f.data)), nil
}

func TestConfigSnapshotPlaneRequiresWorkerAndServesExactDigest(t *testing.T) {
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	service := configSnapshotFixture{digest: digest, data: []byte("snapshot")}
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(),
		WithAuthenticator(&fakeAuthenticator{principal: &Principal{Subject: "worker:test", Issuer: WorkerPrincipalIssuer}}),
		WithConfigSnapshotService(service))
	if err != nil {
		t.Fatal(err)
	}

	current := httptest.NewRecorder()
	handler.ServeHTTP(current, httptest.NewRequest(http.MethodGet, apicontract.ConfigSnapshotPath, nil))
	if current.Code != http.StatusOK || !strings.Contains(current.Body.String(), digest) {
		t.Fatalf("current = %d %s", current.Code, current.Body.String())
	}
	snapshot := httptest.NewRecorder()
	handler.ServeHTTP(snapshot, httptest.NewRequest(http.MethodGet, apicontract.ConfigSnapshotPath+"?digest="+digest, nil))
	if snapshot.Code != http.StatusOK || snapshot.Body.String() != "snapshot" || snapshot.Header().Get(apicontract.DigestHeader) != digest {
		t.Fatalf("snapshot = %d %q headers=%v", snapshot.Code, snapshot.Body.String(), snapshot.Header())
	}
}

func TestConfigSnapshotPlaneRejectsPodPrincipal(t *testing.T) {
	handler, err := NewHandler(&fakeReader{}, RequireRoles(), discardLogger(),
		WithAuthenticator(&fakeAuthenticator{principal: &Principal{Subject: "run", Issuer: PodPrincipalIssuer}}),
		WithConfigSnapshotService(configSnapshotFixture{}))
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, apicontract.ConfigSnapshotPath, nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("pod config snapshot = %d, want 403", response.Code)
	}
}
