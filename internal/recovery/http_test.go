package recovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPArchiveSourceVerifiedDownload(t *testing.T) {
	for _, mode := range []string{"valid", "corrupt", "foreign-repository", "own-run", "expired", "refused", "consumer-error"} {
		t.Run(mode, func(t *testing.T) {
			record := storageTestRecord()
			record.CreatedAt = time.Now().Add(-time.Hour).UTC()
			record.RetainUntil = time.Now().Add(time.Hour).UTC()
			key := record.RepositoryKey
			if mode == "foreign-repository" {
				record.RepositoryKey = "github|||other|repo|"
			}
			if mode == "expired" {
				record.RetainUntil = time.Now().Add(-time.Minute).UTC()
			}
			archive := []byte(expectedBundleHeader(record) + "binary\x00fixture")
			record.ArchiveBytes = int64(len(archive))
			record.ArchiveDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(archive))
			path := filepath.Join(t.TempDir(), BundleFileName)
			if err := os.WriteFile(path, archive, 0o600); err != nil {
				t.Fatal(err)
			}
			var envelope bytes.Buffer
			if err := WriteArchiveEnvelope(context.Background(), path, record, 4096, &envelope); err != nil {
				t.Fatal(err)
			}
			data := envelope.Bytes()
			if mode == "corrupt" {
				data[len(data)-1] ^= 1
			}
			runID := "receiving-run"
			if mode == "own-run" {
				runID = record.RunID
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/runs/"+runID+"/recovery" || r.URL.Query().Get("repositoryKey") != key || r.URL.Query().Get("issue") != "7" || r.Header.Get("Authorization") != "Bearer claims-secret" {
					t.Error("download request lost its claim identity or bearer")
				}
				if mode == "refused" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				_, _ = w.Write(data)
			}))
			defer server.Close()
			source := HTTPArchiveSource{BaseURL: server.URL, Token: "claims-secret", RunID: runID}
			consumedPath := ""
			consumerErr := errors.New("consumer failed")
			err := source.WithArchive(context.Background(), key, "7", func(got Record, path string) error {
				consumedPath = path
				gotBytes, err := os.ReadFile(path)
				if err != nil || got != record || !bytes.Equal(gotBytes, archive) {
					t.Fatalf("download changed verified state: %v", err)
				}
				if mode == "consumer-error" {
					return consumerErr
				}
				return nil
			})
			if mode == "valid" || mode == "consumer-error" {
				if consumedPath == "" || (mode == "valid" && err != nil) || (mode == "consumer-error" && !errors.Is(err, consumerErr)) {
					t.Fatalf("consumer result: path=%q err=%v", consumedPath, err)
				}
				if _, err := os.Stat(filepath.Dir(consumedPath)); !os.IsNotExist(err) {
					t.Fatalf("download staging survived consumer exit: %v", err)
				}
			} else if err == nil || consumedPath != "" {
				t.Fatalf("unusable archive reached consumer: %q %v", consumedPath, err)
			}
		})
	}
}

func TestHTTPArchiveSourceDoesNotFollowRedirects(t *testing.T) {
	var reached atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	source := HTTPArchiveSource{BaseURL: server.URL, Token: "claims-secret", RunID: "receiving-run"}
	err := source.WithArchive(context.Background(), storageTestRecord().RepositoryKey, "7", func(Record, string) error {
		t.Fatal("redirect reached consumer")
		return nil
	})
	if err == nil || reached.Load() {
		t.Fatalf("redirect followed: %v", err)
	}
}

func TestHTTPArchiveSourceCancellationStopsBodyRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	source := HTTPArchiveSource{BaseURL: server.URL, Token: "claims-secret", RunID: "receiving-run"}
	err := source.WithArchive(ctx, storageTestRecord().RepositoryKey, "7", func(Record, string) error {
		t.Fatal("cancelled download reached consumer")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}
