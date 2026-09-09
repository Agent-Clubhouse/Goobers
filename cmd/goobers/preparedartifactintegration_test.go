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
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
)

// This crosses the actual pod recorder, authenticated bounded HTTP routes,
// daemon writer constructor, blob store, and durable run journal. Prepared sets
// admit empty and 16 MiB members; neither can disappear behind a valid pointer.
func TestPodPreparedSetDurablyReachesDaemonJournal(t *testing.T) {
	layout, maxJournalBody := preparedArtifactDaemonFixture(t)
	workspace := t.TempDir()
	payloads := map[string][]byte{
		"empty.txt":         {},
		"large.txt":         bytes.Repeat([]byte("x"), artifactset.MaxPayloadBytes),
		"artifact-set.json": []byte("a legal semantic member sharing the index name"),
	}
	manifest := artifactset.Manifest{SchemaVersion: artifactset.SchemaVersion}
	for name, data := range payloads {
		if err := os.WriteFile(filepath.Join(workspace, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		manifest.Entries = append(manifest.Entries, artifactset.ManifestEntry{Name: name, Path: name, MediaType: "text/plain"})
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := artifactset.Prepare(context.Background(), workspace, "manifest.json", artifactset.NewSanitizer(journal.NewPatternScrubber()))
	if err != nil {
		t.Fatal(err)
	}
	recorder := podArtifactRecorder{stderr: io.Discard, scrubber: journal.NewPatternScrubber()}
	record := func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
		var ref journal.Ref
		var err error
		if durable, ok := any(recorder).(interface {
			RecordPreparedArtifact(context.Context, string, string, []byte) (journal.Ref, error)
		}); ok {
			ref, err = durable.RecordPreparedArtifact(context.Background(), "implement/"+name, media, data)
		} else {
			ref, err = recorder.RecordArtifact("implement/"+name, data)
		}
		return apiv1.ArtifactPointer{Path: ref.Path, Digest: ref.Digest, Size: ref.Size, MediaType: media, Integrity: apiv1.IntegrityDerived}, err
	}
	pointers, err := prepared.Publish(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	if len(pointers) != len(payloads)+1 {
		t.Fatalf("published %d pointers", len(pointers))
	}
	runDir := filepath.Join(layout.ForGaggle("web").RunsDir(), "prepared-run")
	for _, pointer := range pointers {
		stored, err := os.ReadFile(filepath.Join(runDir, pointer.Path))
		if err != nil || int64(len(stored)) != pointer.Size || apiv1.Digest(stored) != pointer.Digest {
			t.Fatalf("published pointer is not durable: size=%d, read=%v", pointer.Size, err)
		}
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		t.Fatal(err)
	}
	before, err := reader.Events()
	if err != nil {
		t.Fatal(err)
	}
	artifacts, sameName := 0, 0
	for _, event := range before {
		if event.Type != journal.EventArtifactRecorded {
			continue
		}
		artifacts++
		if event.Name == "implement/implement/artifact-set.json" {
			sameName++
		}
	}
	if artifacts != 4 || sameName != 2 {
		t.Fatalf("artifact records=%d, same-name member/index records=%d", artifacts, sameName)
	}
	if _, err := prepared.Publish(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	after, err := reader.Events()
	if err != nil || len(after) != len(before) {
		t.Fatalf("redelivery duplicated durable records: before=%d after=%d err=%v", len(before), len(after), err)
	}
	if size := maxJournalBody.Load(); size <= 0 || size > 4096 {
		t.Fatalf("journal requests must carry bounded references, largest body=%d", size)
	}
}

func preparedArtifactDaemonFixture(t *testing.T) (instance.Layout, *atomic.Int64) {
	t.Helper()
	layout := instance.NewLayout(t.TempDir())
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{ObjectMeta: metav1.ObjectMeta{Name: "web"}}}}
	cfg := &instance.Config{Engine: &instance.EngineConfig{HostPort: "127.0.0.1:7233", Namespace: "default", TaskQueue: "q"}}
	blobs, err := blobstore.NewDir(layout.BlobStoreDir())
	if err != nil {
		t.Fatal(err)
	}
	writer, err := newLiveJournalWriter(layout, cfg, set, nil, nil, blobs, nil)
	if err != nil || writer == nil {
		t.Fatalf("daemon writer: %v", err)
	}
	t.Cleanup(writer.Close)
	if _, err := writer.Emit(context.Background(), liveOpenBatch("prepared-run", "web", time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	registry := podauth.NewRegistry()
	authenticator, err := podauth.NewAuthenticator(registry, httpapi.DenyAllAuthenticator{})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.NewHandler(&telemetryParityReader{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0),
		httpapi.WithAuthenticator(authenticator), httpapi.WithBlobService(blobs), httpapi.WithJournalService(writer))
	if err != nil {
		t.Fatal(err)
	}
	var maxBody atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/journal/emit") {
			for old := maxBody.Load(); r.ContentLength > old && !maxBody.CompareAndSwap(old, r.ContentLength); old = maxBody.Load() {
			}
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	token, err := registry.Mint("prepared-run", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range dispatcher.DispatcherControlEnv {
		t.Setenv(key, "")
	}
	stampPodSpanEnv(t, server.URL, server.URL, token, "prepared-run")
	t.Setenv(dispatcher.EnvGaggle, "web")
	t.Setenv(dispatcher.EnvPodAttempt, "3")
	return layout, &maxBody
}
