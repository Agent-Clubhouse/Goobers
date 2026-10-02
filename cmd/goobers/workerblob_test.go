package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/workerhost"
)

func workerBlobDaemon(t *testing.T, root, directory string) (*httptest.Server, *blobstore.Dir) {
	t.Helper()
	store, err := blobstore.NewDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	signer, err := podTokenMinter(cfg)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := podauth.NewAuthenticator(signer, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := readservice.NewLocal(readservice.LocalSources{Definitions: &instance.ConfigSet{Manifest: &apiv1.Manifest{}}}, func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	plane, err := dispatcher.NewSurrenderDir(filepath.Join(directory, "surrender"))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(reader, httpapi.RequireRoles(), log.New(io.Discard, "", 0), httpapi.WithAuthenticator(auth), httpapi.WithBlobService(store), httpapi.WithSurrenderService(plane))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server, store
}

func TestRunWorkerEndpointMaterializesAndRecordsThroughDaemon(t *testing.T) {
	for _, environment := range []bool{false, true} {
		t.Run(fmt.Sprint(environment), func(t *testing.T) {
			root := initDemo(t)
			configureDispatchAuthority(t, root)
			server, daemonStore := workerBlobDaemon(t, root, t.TempDir())
			t.Setenv("GOOBERS_BLOB_STORE", "")
			t.Setenv("GOOBERS_BLOB_ENDPOINT", "")
			// A pod token must never substitute for the worker-only blob credential.
			t.Setenv("GOOBERS_POD_TOKEN", "invalid-pod-credential")
			got := captureWorkerHost(t)
			args := []string{"--instance", root, "--work-root", t.TempDir(), "--config-reload-interval", "0"}
			if environment {
				t.Setenv("GOOBERS_BLOB_ENDPOINT", server.URL)
			} else {
				args = append(args, "--blob-endpoint", server.URL)
			}
			var stdout, stderr bytes.Buffer
			if code := runWorker(args, &stdout, &stderr); code != 0 {
				t.Fatalf("startup %d: %s", code, &stderr)
			}
			// The actual executor seam carries the store selected by the CLI.
			goober, ok := got.Deps.Goober.(workerGoober)
			if !ok {
				t.Fatalf("goober seam %T", got.Deps.Goober)
			}
			store := goober.seams.store
			upstream := []byte("context from another worker")
			digest := fmt.Sprintf("sha256:%x", sha256.Sum256(upstream))
			if err := daemonStore.Put(context.Background(), digest, upstream); err != nil {
				t.Fatal(err)
			}
			stage := t.TempDir()
			pointers := []apiv1.ContextPointer{{Name: "input", Artifact: &apiv1.ArtifactPointer{Path: "artifacts/input.txt", Digest: digest}}}
			if err := workerhost.MaterializeContext(context.Background(), store, stage, pointers); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(stage, "artifacts/input.txt"))
			if err != nil || !bytes.Equal(data, upstream) {
				t.Fatalf("materialize: %q %v", data, err)
			}
			artifacts := workerhost.NewStagingArtifacts(stage, nil, store)
			ref, err := artifacts.RecordArtifact("result", []byte("worker output"))
			if err != nil {
				t.Fatal(err)
			}
			data, err = daemonStore.Get(context.Background(), ref.Digest)
			if err != nil || string(data) != "worker output" {
				t.Fatalf("artifact PUT: %q %v", data, err)
			}
		})
	}
}

func TestRunWorkerBlobModeRefusals(t *testing.T) {
	t.Setenv("GOOBERS_BLOB_ENDPOINT", "")
	t.Setenv("GOOBERS_BLOB_STORE", "")
	for _, flags := range [][]string{nil, {"--blob-store", "dir", "--blob-endpoint", "http://plane"}} {
		var out, errs bytes.Buffer
		code := runWorker(append([]string{"--instance", "unused"}, flags...), &out, &errs)
		if code != 2 || !strings.Contains(errs.String(), "WORKER_BLOB_MODE") {
			t.Fatalf("exit %d: %s", code, &errs)
		}
	}
}

func TestRunWorkerDispatchRefusesUnsharedDirectory(t *testing.T) {
	root := initDemo(t)
	configureDispatchAuthority(t, root)
	server, _ := workerBlobDaemon(t, root, t.TempDir())
	t.Setenv("GOOBERS_BLOB_ENDPOINT", server.URL)
	got := captureWorkerHost(t)
	var out, errs bytes.Buffer
	code := runWorker([]string{"--instance", root, "--work-root", t.TempDir(), "--blob-store", t.TempDir(), "--daemon-api", server.URL, "--dispatch-namespace", "stages"}, &out, &errs)
	if code != 1 || !strings.Contains(errs.String(), "WORKER_BLOB_STORE_MISMATCH") {
		t.Fatalf("exit %d: %s", code, &errs)
	}
	if len(got.TaskQueues) != 0 {
		t.Fatal("mismatched store reached polling host")
	}
}
