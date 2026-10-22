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

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/configmirror"
	"github.com/goobers/goobers/internal/instance"
)

func TestWorkerFetchesConfigByDigest(t *testing.T) {
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
			_, _ = fmt.Fprintf(w, `{"digest":%q}`, plane.Current())
			return
		}
		if requested != digest {
			t.Fatalf("requested digest = %q, want %q", requested, digest)
		}
		archiveGets.Add(1)
		file, err := plane.Open(requested)
		if err != nil {
			http.NotFound(w, request)
			return
		}
		defer func() { _ = file.Close() }()
		w.Header().Set(apicontract.DigestHeader, requested)
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
