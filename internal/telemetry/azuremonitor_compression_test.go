package telemetry

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestAzureMonitorCompressionReuse(t *testing.T) {
	cache := newAzureGzipCache(4)
	var wg sync.WaitGroup
	for worker := range 12 {
		wg.Go(func() {
			for iteration := range 16 {
				raw := bytes.Repeat([]byte(fmt.Sprintf("worker=%d iteration=%d\x00\xff", worker, iteration)), iteration*128)
				var output bytes.Buffer
				if err := cache.compress(&output, raw); err != nil {
					t.Error(err)
					return
				}
				reader, err := gzip.NewReader(bytes.NewReader(output.Bytes()))
				if err != nil {
					t.Error(err)
					return
				}
				decoded, readErr := io.ReadAll(reader)
				closeErr := reader.Close()
				if readErr != nil || closeErr != nil || !bytes.Equal(decoded, raw) {
					t.Errorf("reused compressor mixed or corrupted payloads: read=%v close=%v", readErr, closeErr)
					return
				}
			}
		})
	}
	wg.Wait()
	if len(cache.idle) > 4 {
		t.Fatal("compressor retention exceeded its bound")
	}
}

func TestAzureMonitorCompressionRetentionBound(t *testing.T) {
	cache := newAzureGzipCache(2)
	writers := make([]*gzip.Writer, 8)
	for i := range writers {
		// More concurrent borrowers than retained capacity must never wait.
		writers[i] = cache.take(io.Discard)
		if _, err := writers[i].Write([]byte("initialize compressor")); err != nil {
			t.Fatal(err)
		}
	}
	for _, writer := range writers {
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		cache.release(writer)
	}
	if len(cache.idle) != cap(cache.idle) || cap(cache.idle) != 2 {
		t.Fatalf("retained %d compressors, capacity %d", len(cache.idle), cap(cache.idle))
	}
}

type failedCompressionWriter struct{ err error }

func (w failedCompressionWriter) Write([]byte) (int, error) { return 0, w.err }

func TestAzureMonitorCompressionRecoversFromWriteFailure(t *testing.T) {
	cache := newAzureGzipCache(1)
	failure := errors.New("synthetic destination write failure")
	if err := cache.compress(failedCompressionWriter{failure}, []byte("failed payload")); !errors.Is(err, failure) {
		t.Fatalf("write failure lost: %v", err)
	}
	var output bytes.Buffer
	if err := cache.compress(&output, []byte("next independent payload")); err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(&output)
	if err != nil {
		t.Fatal(err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(got) != "next independent payload" {
		t.Fatalf("failed compressor contaminated next request: read=%v close=%v body=%q", readErr, closeErr, got)
	}
}

// Isolate the compression/request path from journal fsync and envelope encoding.
// The receiver drains the compressed bytes without decompression, so its gzip
// reader allocations cannot be mistaken for client-side allocation pressure.
// This is a component measurement, not an Azure service throughput guarantee.
func BenchmarkAzureMonitorCompression(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Encoding") != "gzip" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	httpClient := server.Client()
	httpClient.Timeout = 5 * time.Second
	client := &azureMonitorClient{ingestionURL: server.URL, httpClient: httpClient}
	for _, size := range []int{1024, 32 << 10} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			raw := bytes.Repeat([]byte("x"), size)
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for b.Loop() {
				if err := client.sendPayloadRequest(b.Context(), raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
