package scheduling

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/stateclient"
)

type unusedReader struct{ readservice.Reader }
type statePlane struct {
	server   *httptest.Server
	layout   instance.Layout
	registry *podauth.Registry
}

func testFileLock(path string, fn func() error) error {
	held, err := lock.TryAcquire(path)
	if err != nil {
		return err
	}
	defer func() { _ = held.Release() }()
	return fn()
}
func testNamedLock(path, _ string, fn func() error) error { return testFileLock(path, fn) }
func testStateFiles() StateFiles {
	return StateFiles{ClaimLock: testNamedLock, FileLock: testFileLock, BoundedLock: testNamedLock}
}
func newStatePlane(t *testing.T) *statePlane {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := testStateFiles().NewService(layout)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&unusedReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator),
		httpapi.WithStateService(service),
	)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return &statePlane{server: server, layout: layout, registry: registry}
}

// admitRun creates runID's journal under gaggle on the daemon (the run.yaml
// the live writer creates at a run's first emit — the authority the plane's
// containment check consults) and mints its pod token.
func (p *statePlane) admitRun(t *testing.T, gaggle, runID string) string {
	t.Helper()
	run, err := journal.Create(p.layout.ForGaggle(gaggle).RunsDir(), journal.RunIdentity{RunID: runID, Workflow: "implementation", Gaggle: gaggle}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
	token, err := p.registry.Mint(runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// client is the plane backend a stage pod for gaggle would construct.
func (p *statePlane) client(t *testing.T, gaggle, token string) stateclient.Store {
	t.Helper()
	client, err := stateclient.NewHTTP(stateclient.HTTPConfig{BaseURL: p.server.URL, Token: token, Gaggle: gaggle})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// hexDigest builds a syntactically valid scan-cursor digest from a label, so
// tests name distinct cursors without depending on a real scan key.
func hexDigest(label string) string {
	digest := ""
	for len(digest) < 64 {
		for _, r := range label {
			digest += strconv.FormatInt(int64(r%16), 16)
			if len(digest) == 64 {
				break
			}
		}
	}
	return digest
}

// TestStatePlaneContainsAPodToItsOwnGaggle is R3's containment end to end,
// against the real daemon service: the authority is the caller's own run.yaml,
// and a gaggle it cannot be verified against is refused rather than resolved.
func TestStatePlaneContainsAPodToItsOwnGaggle(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-1")

	own := plane.client(t, "goobers", token)
	if _, err := own.Put(t.Context(), stateclient.KeyBlockedRecords, []byte("{}"), ""); err != nil {
		t.Fatalf("own-gaggle write: %v", err)
	}

	foreign := plane.client(t, "other", token)
	_, err := foreign.Get(t.Context(), stateclient.KeyBlockedRecords)
	var planeErr *stateclient.Error
	if !errors.As(err, &planeErr) || planeErr.Status != http.StatusForbidden {
		t.Fatalf("foreign-gaggle read err = %v, want a typed 403", err)
	}
	if _, err := foreign.Put(t.Context(), stateclient.KeyBlockedRecords, []byte("{}"), ""); !errors.As(err, &planeErr) || planeErr.Status != http.StatusForbidden {
		t.Fatalf("foreign-gaggle write err = %v, want a typed 403", err)
	}

	// A gaggle that is not one plain path element is refused at construction
	// rather than resolved into a traversal. It has to be refused HERE: once
	// such a path reaches the wire the client normalizes it into a request
	// against a different route, whose 404 is indistinguishable from the 404
	// an absent key answers with — a traversal would read back as "no value".
	for _, gaggle := range []string{"..", ".", "a/b", "", "goobers/../other"} {
		hostile, err := stateclient.NewHTTP(stateclient.HTTPConfig{
			BaseURL: plane.server.URL, Token: token, Gaggle: gaggle,
		})
		if err == nil {
			t.Fatalf("gaggle %q was accepted: %#v", gaggle, hostile)
		}
	}
}

// TestStatePlaneRefusesKeysOutsideTheNamespaceServerSide keeps the containment
// from depending on a well-behaved client: the daemon refuses too.
func TestStatePlaneRefusesKeysOutsideTheNamespaceServerSide(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-1")

	// Write a file the plane must never serve, right next to the ones it does.
	secret := filepath.Join(plane.layout.SchedulerDir(), "claims.json")
	if err := os.WriteFile(secret, []byte(`{"claims":"secret"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		plane.server.URL+"/api/v1/gaggles/goobers/state/claims.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want the key refused", response.StatusCode)
	}
	body, _ := io.ReadAll(response.Body)
	if string(body) != "" && string(body) == `{"claims":"secret"}` {
		t.Fatal("the plane served a file outside the scheduler-state namespace")
	}
}

// TestStageStateStoreSelectionFailsClosed pins the stage seam: a pod that has
// an endpoint but no bearer must never silently fall through to a local
// scheduler-state file nothing will read.
func TestStageStateStoreSelectionFailsClosed(t *testing.T) {
	plane := newStatePlane(t)
	token := plane.admitRun(t, "goobers", "run-1")

	store, err := testStateFiles().Stage(plane.layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.(*stateclient.File); !ok {
		t.Fatalf("store = %T with no plane env, want the file backend", store)
	}

	t.Setenv(stateclient.EnvEndpoint, plane.server.URL)
	if _, err := testStateFiles().Stage(plane.layout); !errors.Is(err, stateclient.ErrEndpointWithoutToken) {
		t.Fatalf("err = %v, want ErrEndpointWithoutToken", err)
	}
	t.Setenv(stateclient.EnvToken, token)
	if _, err := testStateFiles().Stage(plane.layout); !errors.Is(err, stateclient.ErrEndpointWithoutGaggle) {
		t.Fatalf("err = %v, want ErrEndpointWithoutGaggle", err)
	}
	t.Setenv(stateclient.EnvGaggle, "goobers")
	store, err = testStateFiles().Stage(plane.layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store.(*stateclient.HTTP); !ok {
		t.Fatalf("store = %T with the plane env stamped, want the plane backend", store)
	}
	if !StatePlaneSelected() {
		t.Fatal("statePlaneSelected did not report the stamped endpoint")
	}
}

// TestSchedulerStateKeysKeepTheirExistingLocks is finding 002's explicit
// correction, pinned: the plane must be served under the SAME lock each key
// already used, not under a new one. blocked.json, the backlog scan cursors
// and pr-select's fairness lease ride claims.lock (blockedrecords.go,
// backlogquery.go, prselectfairness.go); the reconcile ledger
// and the sibling cache keep their own. Splitting the lock domain would let a
// plane write and an in-process write race the very updates the CAS exists to
// protect.
func TestSchedulerStateKeysKeepTheirExistingLocks(t *testing.T) {
	layout := instance.NewLayout(t.TempDir())
	if err := os.MkdirAll(layout.SchedulerDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := testStateFiles().Lock(layout)

	cases := []struct {
		key  string
		file string
	}{
		{stateclient.KeyBlockedRecords, "claims.lock"},
		{stateclient.ScanCursorKey(hexDigest("scan")), "claims.lock"},
		{stateclient.KeyPRSelectFairness, "claims.lock"},
		{stateclient.KeyPostMergeReconcileLedger, "post-merge-reconcile.lock"},
		{stateclient.KeySiblingContextCache, "sibling-context-cache.lock"},
	}
	for _, testCase := range cases {
		t.Run(testCase.key, func(t *testing.T) {
			if err := lock(testCase.key, "probe", func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			// The lock the section took is the lock file it created.
			if _, err := os.Stat(filepath.Join(layout.SchedulerDir(), testCase.file)); err != nil {
				t.Fatalf("%s did not take %s: %v", testCase.key, testCase.file, err)
			}
		})
	}
}
