package temporalcli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
	cancel context.CancelFunc
}

func (c *fakeClock) sleep(ctx context.Context, delay time.Duration) error {
	c.sleeps = append(c.sleeps, delay)
	c.now = c.now.Add(delay)
	if c.cancel != nil {
		c.cancel()
	}
	return ctx.Err()
}

func retryDownloader(client *http.Client, clock *fakeClock) *downloader {
	d := newDownloader()
	d.client = client
	d.now = func() time.Time { return clock.now }
	d.sleep = clock.sleep
	d.jitter = func(time.Duration) time.Duration { return 0 }
	return d
}

func TestGetRetryClassification(t *testing.T) {
	tests := []struct {
		name         string
		firstStatus  int
		retryAfter   string
		wantRequests int
		wantSleeps   []time.Duration
		wantErr      bool
	}{
		{
			name:         "429 retries and honors Retry-After",
			firstStatus:  http.StatusTooManyRequests,
			retryAfter:   "3",
			wantRequests: 2,
			wantSleeps:   []time.Duration{3 * time.Second},
		},
		{
			name:         "500 retries",
			firstStatus:  http.StatusInternalServerError,
			wantRequests: 2,
			wantSleeps:   []time.Duration{baseDelay / 2},
		},
		{
			name:         "404 is permanent",
			firstStatus:  http.StatusNotFound,
			wantRequests: 1,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				if requests == 1 {
					if tt.retryAfter != "" {
						w.Header().Set("Retry-After", tt.retryAfter)
					}
					http.Error(w, http.StatusText(tt.firstStatus), tt.firstStatus)
					return
				}
				_, _ = io.WriteString(w, "ok")
			}))
			defer server.Close()

			clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
			d := retryDownloader(server.Client(), clock)
			err := d.get(context.Background(), server.URL, consumeAll)
			if (err != nil) != tt.wantErr {
				t.Fatalf("get() error = %v, wantErr %t", err, tt.wantErr)
			}
			if requests != tt.wantRequests {
				t.Errorf("requests = %d, want %d", requests, tt.wantRequests)
			}
			assertDurations(t, clock.sleeps, tt.wantSleeps)
		})
	}
}

func TestGetRetriesTransportAndReadErrors(t *testing.T) {
	t.Run("transport", func(t *testing.T) {
		transport := &sequenceTransport{results: []transportResult{
			{err: errors.New("connection reset")},
			{response: response(http.StatusOK, "ok")},
		}}
		clock := &fakeClock{}
		d := retryDownloader(&http.Client{Transport: transport}, clock)
		if err := d.get(context.Background(), "https://example.invalid", consumeAll); err != nil {
			t.Fatal(err)
		}
		if transport.calls != 2 {
			t.Fatalf("transport calls = %d, want 2", transport.calls)
		}
		assertDurations(t, clock.sleeps, []time.Duration{baseDelay / 2})
	})

	t.Run("read", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			_, _ = io.WriteString(w, "ok")
		}))
		defer server.Close()
		clock := &fakeClock{}
		d := retryDownloader(server.Client(), clock)
		consumes := 0
		err := d.get(context.Background(), server.URL, func(body io.Reader) error {
			consumes++
			if consumes == 1 {
				return io.ErrUnexpectedEOF
			}
			return consumeAll(body)
		})
		if err != nil {
			t.Fatal(err)
		}
		if requests != 2 {
			t.Fatalf("requests = %d, want 2", requests)
		}
		assertDurations(t, clock.sleeps, []time.Duration{baseDelay / 2})
	})
}

func TestGetExhaustsWithBoundedExponentialBackoff(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	clock := &fakeClock{}
	d := retryDownloader(server.Client(), clock)

	err := d.get(context.Background(), server.URL, consumeAll)
	if err == nil || !strings.Contains(err.Error(), "after 4 attempt(s)") {
		t.Fatalf("get() error = %v, want four-attempt exhaustion", err)
	}
	if requests != maxAttempts {
		t.Fatalf("requests = %d, want %d", requests, maxAttempts)
	}
	assertDurations(t, clock.sleeps, []time.Duration{
		baseDelay / 2,
		baseDelay,
		2 * baseDelay,
	})
	for _, delay := range clock.sleeps {
		if delay > maxDelay {
			t.Fatalf("sleep %s exceeds max delay %s", delay, maxDelay)
		}
	}
}

func TestRetryAfterHTTPDateAndCap(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	if got := parseRetryAfter(now.Add(12*time.Second).Format(http.TimeFormat), now); got != 12*time.Second {
		t.Fatalf("HTTP-date Retry-After = %s, want 12s", got)
	}
	if got := parseRetryAfter(now.Add(time.Hour).Format(http.TimeFormat), now); got != maxRetryAfter {
		t.Fatalf("capped HTTP-date Retry-After = %s, want %s", got, maxRetryAfter)
	}
	if got := parseRetryAfter("9999", now); got != maxRetryAfter {
		t.Fatalf("capped seconds Retry-After = %s, want %s", got, maxRetryAfter)
	}
}

func TestGetCancellationMidBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	clock := &fakeClock{cancel: cancel}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "busy", http.StatusTooManyRequests)
	}))
	defer server.Close()
	d := retryDownloader(server.Client(), clock)

	err := d.get(ctx, server.URL, consumeAll)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("get() error = %v, want context cancellation", err)
	}
	if len(clock.sleeps) != 1 {
		t.Fatalf("sleeps = %v, want one interrupted backoff", clock.sleeps)
	}
}

func TestDownloadExtractsArchives(t *testing.T) {
	for _, tt := range []struct {
		name    string
		suffix  string
		archive func(t *testing.T, name string, body []byte) []byte
	}{
		{name: "tar.gz", suffix: ".tar.gz", archive: tarGZArchive},
		{name: "zip", suffix: ".zip", archive: zipArchive},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const executable = "temporal-binary"
			archive := tt.archive(t, "nested/temporal", []byte(executable))
			server := archiveServer(t, tt.suffix, "nested/temporal", archive)
			defer server.Close()
			output := filepath.Join(t.TempDir(), "bin", "temporal")
			d := newDownloader()
			d.client = server.Client()
			d.infoURL = server.URL + "/info"
			if err := d.download(context.Background(), output); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != executable {
				t.Fatalf("installed bytes = %q, want %q", got, executable)
			}
			if runtimeExecutableCheck() {
				info, err := os.Stat(output)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm()&0o111 == 0 {
					t.Fatalf("installed mode = %v, want executable", info.Mode())
				}
			}
		})
	}
}

func TestDownloadArchiveFailuresLeaveNoOutput(t *testing.T) {
	for _, tt := range []struct {
		name    string
		suffix  string
		entry   string
		archive []byte
	}{
		{name: "tar missing entry", suffix: ".tar.gz", entry: "temporal", archive: tarGZArchive(t, "other", []byte("x"))},
		{name: "zip missing entry", suffix: ".zip", entry: "temporal", archive: zipArchive(t, "other", []byte("x"))},
		{name: "corrupt tar", suffix: ".tar.gz", entry: "temporal", archive: []byte("not gzip")},
		{name: "corrupt zip", suffix: ".zip", entry: "temporal", archive: []byte("not zip")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := archiveServer(t, tt.suffix, tt.entry, tt.archive)
			defer server.Close()
			output := filepath.Join(t.TempDir(), "bin", "temporal")
			d := newDownloader()
			d.client = server.Client()
			d.infoURL = server.URL + "/info"
			if err := d.download(context.Background(), output); err == nil {
				t.Fatal("download succeeded, want extraction failure")
			}
			if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("output stat error = %v, want no partial output", err)
			}
		})
	}
}

func consumeAll(body io.Reader) error {
	_, err := io.ReadAll(body)
	return err
}

func assertDurations(t *testing.T, got, want []time.Duration) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("sleeps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sleep[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

type transportResult struct {
	response *http.Response
	err      error
}

type sequenceTransport struct {
	results []transportResult
	calls   int
}

func (s *sequenceTransport) RoundTrip(*http.Request) (*http.Response, error) {
	result := s.results[s.calls]
	s.calls++
	return result.response, result.err
}

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func archiveServer(t *testing.T, suffix, entry string, archive []byte) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/info":
			_, _ = io.WriteString(w, `{"archiveUrl":"`+server.URL+"/archive"+suffix+`","fileToExtract":"`+entry+`"}`)
		case "/archive" + suffix:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	return server
}

func tarGZArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	gz := gzip.NewWriter(&encoded)
	tarWriter := tar.NewWriter(gz)
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func zipArchive(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var encoded bytes.Buffer
	writer := zip.NewWriter(&encoded)
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func runtimeExecutableCheck() bool {
	return os.PathSeparator == '/'
}
