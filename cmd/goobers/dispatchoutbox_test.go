package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
)

func TestPublishPodOutboxScrubsUploadsAndPreservesPaths(t *testing.T) {
	var mu sync.Mutex
	uploaded := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		mu.Lock()
		uploaded[strings.TrimPrefix(r.URL.Path, dispatcher.BlobPathPrefix)] = data
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll("reports/nested", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("reports", "result.txt"), []byte("token=secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("reports", "nested", "trace.log"), []byte("trace"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(dispatcher.EnvStageOutbox, `["reports"]`)
	t.Setenv(dispatcher.EnvBlobEndpoint, server.URL)
	t.Setenv(dispatcher.EnvPodToken, "pod-token")
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte("secret"))

	manifest, err := publishPodOutbox(t.Context(), scrubber)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 2 ||
		manifest[0].RelPath != "reports/nested/trace.log" ||
		manifest[1].RelPath != "reports/result.txt" {
		t.Fatalf("manifest=%+v", manifest)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, file := range manifest {
		data, ok := uploaded[file.Digest]
		if !ok || int64(len(data)) != file.Size || journal.Digest(data) != file.Digest {
			t.Fatalf("uploaded[%s]=%q manifest=%+v", file.Digest, data, file)
		}
		if strings.Contains(string(data), "secret") {
			t.Fatalf("uploaded outbox retained secret: %q", data)
		}
	}
}

func TestPublishPodOutboxFailsClosedWithoutBlobPlane(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile("report.txt", []byte("report"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(dispatcher.EnvStageOutbox, `["report.txt"]`)
	t.Setenv(dispatcher.EnvBlobEndpoint, "")
	if _, err := publishPodOutbox(t.Context(), nil); err == nil {
		t.Fatal("declared outbox succeeded without a blob plane")
	}
}
