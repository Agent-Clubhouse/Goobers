package recovery

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPArchivePublisherRequiresCompleteCustodyAcknowledgement(t *testing.T) {
	for _, mode := range []string{"valid", "refused", "redirect", "corrupt", "foreign-run"} {
		t.Run(mode, func(t *testing.T) {
			record, path := publicationArchiveFixture(t)
			if mode == "corrupt" {
				if err := os.WriteFile(path, []byte("corrupt"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var called atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/runs/"+record.RunID+"/recovery" || r.URL.Query().Get("issue") != "7" || r.Header.Get("Authorization") != "Bearer claims-secret" {
					t.Error("upload lost claims identity")
				}
				if mode == "redirect" {
					w.Header().Set("Location", "/must-not-follow")
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				if mode == "corrupt" {
					data, _ := io.ReadAll(r.Body)
					if len(data) != 0 {
						t.Error("invalid archive sent envelope bytes")
					}
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				got, err := ReceiveArchiveEnvelope(r.Context(), r.Body, t.TempDir(), 4096, nil)
				if err != nil || got != record {
					t.Errorf("upload changed verified envelope: %+v %v", got, err)
				}
				if mode == "refused" {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			publisher := HTTPArchivePublisher{BaseURL: server.URL, Token: "claims-secret", RunID: record.RunID}
			if mode == "foreign-run" {
				publisher.RunID = "another-run"
			}
			err := publisher.PublishArchive(context.Background(), "7", record, path)
			server.Close()
			if (err == nil) != (mode == "valid") {
				t.Fatalf("publication %s: %v", mode, err)
			}
			if mode == "foreign-run" && called.Load() != 0 || called.Load() > 1 {
				t.Fatal("unauthorized or redirected request reached the server")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("publisher removed caller-owned source: %v", err)
			}
		})
	}
}

func TestHTTPArchivePublisherRejectsPrematureSuccess(t *testing.T) {
	record, path := publicationArchiveFixture(t)
	client := &http.Client{Transport: publicationRoundTripper(func(r *http.Request) (*http.Response, error) {
		// Deliberately return success without reading a byte. The publisher must
		// join its blocked writer and refuse this false acknowledgement.
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Header: make(http.Header), Request: r}, nil
	})}
	publisher := HTTPArchivePublisher{BaseURL: "https://example.invalid", Token: "claims-secret", RunID: record.RunID, Client: client}
	if err := publisher.PublishArchive(context.Background(), "7", record, path); err == nil {
		t.Fatal("accepted success without complete upload")
	}
}

func TestHTTPArchivePublisherScrubsTransportFailure(t *testing.T) {
	record, path := publicationArchiveFixture(t)
	client := &http.Client{Transport: publicationRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("credential https://claims-secret@example.invalid/private-archive")
	})}
	publisher := HTTPArchivePublisher{BaseURL: "https://example.invalid", Token: "claims-secret", RunID: record.RunID, Client: client}
	err := publisher.PublishArchive(context.Background(), "7", record, path)
	if err == nil || strings.Contains(err.Error(), "claims-secret") || strings.Contains(err.Error(), "private-archive") {
		t.Fatalf("unsafe transport error: %v", err)
	}
}

type publicationRoundTripper func(*http.Request) (*http.Response, error)

func (f publicationRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func publicationArchiveFixture(t *testing.T) (Record, string) {
	t.Helper()
	record := storageTestRecord()
	archive := []byte(expectedBundleHeader(record) + "binary\x00fixture")
	record.ArchiveBytes = int64(len(archive))
	record.ArchiveDigest = fmt.Sprintf("sha256:%x", sha256.Sum256(archive))
	path := filepath.Join(t.TempDir(), BundleFileName)
	if err := os.WriteFile(path, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	return record, path
}
