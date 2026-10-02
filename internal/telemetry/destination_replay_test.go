package telemetry

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func namedAzure(name, endpoint, root string) NamedDestination {
	return NamedDestination{Name: name, Config: Config{AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + endpoint, AzureMonitorTraces: true, AzureMonitorReplayRoot: root, AzureMonitorReplayMaxAge: time.Hour, AzureMonitorReplayMaxBytes: 1 << 20}}
}

func TestNamedAzureReplayRootsRemainIndependentAcrossReorderAndRestart(t *testing.T) {
	var available atomic.Bool
	failed := make(chan string, 32)
	replayed := make(chan string, 32)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "gzip", 400)
			return
		}
		defer func() { _ = reader.Close() }()
		body, err := io.ReadAll(reader)
		if err != nil {
			http.Error(w, "body", 400)
			return
		}
		if !available.Load() {
			failed <- string(body)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		replayed <- string(body)
		w.WriteHeader(http.StatusOK)
	}))
	defer bad.Close()
	healthy, good := azureMonitorTestServer(t)
	defer good.Close()
	root := t.TempDir()
	badRoot, goodRoot := filepath.Join(root, "bad"), filepath.Join(root, "good")
	first, second := namedAzure("bad", bad.URL, badRoot), namedAzure("good", good.URL, goodRoot)
	c, err := New(t.Context(), Config{Destinations: []NamedDestination{first, second}})
	if err != nil {
		t.Fatal(err)
	}
	emitNamedRun(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	_ = c.Shutdown(ctx)
	cancel()
	failedBody := receiveNamed(t, failed)
	receiveNamed(t, healthy)
	if stats := InspectAzureReplayRoot(badRoot); stats.PendingRecords != 1 {
		t.Fatalf("failed destination lost spool: %+v", stats)
	}
	if stats := InspectAzureReplayRoot(goodRoot); stats.PendingRecords != 0 {
		t.Fatalf("healthy destination retained duplicate: %+v", stats)
	}
	available.Store(true)
	c, err = New(t.Context(), Config{Destinations: []NamedDestination{second, first}})
	if err != nil {
		t.Fatal(err)
	}
	replayedBody := receiveNamed(t, replayed)
	closeNamedClient(t, c)
	if a, b := azureRecordID(failedBody), azureRecordID(replayedBody); a == "" || a != b {
		t.Fatalf("replay changed stable identity: %q %q", a, b)
	}
	select {
	case <-healthy:
		t.Fatal("healthy destination was replayed twice")
	default:
	}
	if stats := InspectAzureReplayRoot(badRoot); stats.PendingRecords != 0 || stats.LastSuccess.IsZero() || stats.ActiveFailure {
		t.Fatalf("recovery not retained: %+v", stats)
	}
}

func TestNamedDestinationsRejectSharedReplayRoot(t *testing.T) {
	root := t.TempDir()
	_, err := New(t.Context(), Config{Destinations: []NamedDestination{namedAzure("one", "http://127.0.0.1:1", root), namedAzure("two", "http://127.0.0.1:2", filepath.Join(root, "."))}})
	if err == nil {
		t.Fatal("shared replay root was accepted")
	}
}
