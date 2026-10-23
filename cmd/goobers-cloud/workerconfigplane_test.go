package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/configmirror"
	"github.com/goobers/goobers/internal/instance"
)

func TestWorkerFetchesConfigByDigest(t *testing.T) {
	const instanceID = "0123456789abcdef0123456789abcdef"
	source := initDemo(t)
	layout := instance.NewLayout(source)
	document, err := os.ReadFile(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	plane, err := configmirror.NewPlane(filepath.Join(t.TempDir(), "plane"))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := plane.PublishValidated(t.Context(), layout.ConfigDir(), document, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	var archiveGets atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer goobers-worker.") {
			t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
		}
		requested := request.URL.Query().Get("digest")
		if requested == "" {
			_, _ = fmt.Fprintf(w, `{"digest":%q,"instanceID":%q}`, plane.Current(), instanceID)
			return
		}
		if requested != digest {
			t.Fatalf("requested digest = %q, want %q", requested, digest)
		}
		archiveGets.Add(1)
		file, size, err := plane.Open(requested)
		if err != nil {
			http.NotFound(w, request)
			return
		}
		defer func() { _ = file.Close() }()
		w.Header().Set(apicontract.DigestHeader, requested)
		w.Header().Set("Content-Length", fmt.Sprint(size))
		_, _ = io.Copy(w, file)
	}))
	t.Cleanup(server.Close)
	keyFile := filepath.Join(t.TempDir(), "worker-key")
	if err := os.WriteFile(keyFile, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	got := captureWorkerHost(t)
	var stdout, stderr bytes.Buffer
	code := runWorker([]string{
		"--config-endpoint", server.URL,
		"--config-key-file", keyFile,
		"--work-root", t.TempDir(),
		"--blob-store", t.TempDir(),
		"--config-reload-interval", "0",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit = %d\nstderr: %s", code, stderr.String())
	}
	if archiveGets.Load() != 1 {
		t.Fatalf("archive GETs = %d, want 1", archiveGets.Load())
	}
	if got.TaskQueues == nil || got.Deps.Goober == nil {
		t.Fatal("fetched configuration did not reach worker runtime wiring")
	}
}

func TestWorkerConfigPlaneRetainsSupersededPin(t *testing.T) {
	firstRoot := initDemo(t)
	seams := workerReloadSeams(t, firstRoot)
	pin := currentPin(t, seams)
	if _, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, pin); err != nil {
		t.Fatal(err)
	}

	secondRoot := initDemo(t)
	editInstructions(t, secondRoot, "replacement instructions")
	outcome, err := seams.reloadFromLayout(instance.NewLayout(secondRoot))
	if err != nil || !outcome.Applied {
		t.Fatalf("remote reload = %+v, %v", outcome, err)
	}
	if _, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, pin); err != nil {
		t.Fatalf("superseded pin became gate_pin_missing: %v", err)
	}
}

type workerConfigPlaneFixture struct {
	plane      *configmirror.Plane
	server     *httptest.Server
	source     string
	keyFile    string
	instanceID string
}

func newWorkerConfigPlaneFixture(t *testing.T) *workerConfigPlaneFixture {
	t.Helper()
	fixture := &workerConfigPlaneFixture{
		source:     initDemo(t),
		keyFile:    filepath.Join(t.TempDir(), "worker-key"),
		instanceID: "0123456789abcdef0123456789abcdef",
	}
	if err := os.WriteFile(fixture.keyFile, []byte(strings.Repeat("k", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	fixture.plane, err = configmirror.NewPlane(filepath.Join(t.TempDir(), "plane"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requested := request.URL.Query().Get("digest")
		if requested == "" {
			_, _ = fmt.Fprintf(w, `{"digest":%q,"instanceID":%q}`, fixture.plane.Current(), fixture.instanceID)
			return
		}
		file, size, err := fixture.plane.Open(requested)
		if err != nil {
			http.NotFound(w, request)
			return
		}
		defer func() { _ = file.Close() }()
		w.Header().Set(apicontract.DigestHeader, requested)
		w.Header().Set("Content-Length", fmt.Sprint(size))
		_, _ = io.Copy(w, file)
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *workerConfigPlaneFixture) publish(t *testing.T) string {
	t.Helper()
	layout := instance.NewLayout(f.source)
	document, err := os.ReadFile(layout.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	digest, err := f.plane.PublishValidated(t.Context(), layout.ConfigDir(), document, func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestWorkerPlaneReplicasShareDaemonIdentity(t *testing.T) {
	fixture := newWorkerConfigPlaneFixture(t)
	fixture.publish(t)
	for range 2 {
		setup, err := prepareWorkerConfigPlane(fixture.server.URL, fixture.keyFile, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if setup.root == setup.snapshotRoot {
			t.Fatal("immutable snapshot became the writable instance root")
		}
		if got, err := instance.ReadRootIdentity(setup.root); err != nil || got != fixture.instanceID {
			t.Fatalf("replica identity = %q, %v; want daemon %q", got, err, fixture.instanceID)
		}
	}
}

func TestWorkerConfigPlaneRetriesInterruptedSnapshotTransfer(t *testing.T) {
	fixture := newWorkerConfigPlaneFixture(t)
	digest := fixture.publish(t)
	file, _, err := fixture.plane.Open(digest)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	var transfers atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("digest") == "" {
			_, _ = fmt.Fprintf(w, `{"digest":%q,"instanceID":%q}`, digest, fixture.instanceID)
			return
		}
		w.Header().Set(apicontract.DigestHeader, digest)
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if transfers.Add(1) == 1 {
			_, _ = w.Write(data[:len(data)/2])
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	if _, err := prepareWorkerConfigPlane(server.URL, fixture.keyFile, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if transfers.Load() != 2 {
		t.Fatalf("snapshot transfers = %d, want interrupted plus retry", transfers.Load())
	}
}

func TestWorkerConfigPlaneRotationsPreserveStableRootAndLeasedSnapshot(t *testing.T) {
	fixture := newWorkerConfigPlaneFixture(t)
	fixture.publish(t)
	workRoot := t.TempDir()
	setup, err := prepareWorkerConfigPlane(fixture.server.URL, fixture.keyFile, workRoot)
	if err != nil {
		t.Fatal(err)
	}
	seams, err := newWorkerSeams(setup.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := configureWorkerConfigSeams(seams, 3, setup); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(setup.root, "worker-config-generations", "in-use")
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("leased"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, release, err := seams.snapshotForInvocation(t.Context(), apiv1.InvocationEnvelope{})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	leasedRoot := filepath.Dir(snapshot.ConfigDirectory)
	cacheRoot := filepath.Join(workRoot, "config-snapshots")
	for generation := 1; generation <= 5; generation++ {
		editInstructions(t, fixture.source, fmt.Sprintf("generation %d", generation))
		fixture.publish(t)
		current, err := setup.client.Current(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		root, err := setup.client.Materialize(t.Context(), cacheRoot, current.Digest, runtimewiring.ValidateSeededWorkerConfig)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := seams.reloadFromLayout(instance.NewLayout(root)); err != nil {
			t.Fatal(err)
		}
		if err := pruneWorkerConfigCache(cacheRoot, seams.retainedConfigRoots()); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := instance.ReadRootIdentity(setup.root); err != nil || got != fixture.instanceID {
		t.Fatalf("stable root identity = %q, %v", got, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "leased" {
		t.Fatalf("stable root marker = %q, %v", data, err)
	}
	if _, err := os.Stat(leasedRoot); err != nil {
		t.Fatalf("leased snapshot was pruned: %v", err)
	}
}
