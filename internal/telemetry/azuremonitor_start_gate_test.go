package telemetry

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAzureReplayStartGateConfigCoversAllStreams(t *testing.T) {
	start := make(chan struct{})
	cfg := Config{AzureMonitorReplayRoot: t.TempDir(), AzureMonitorReplayStart: start,
		AzureMonitorReplayMaxAge: time.Hour, AzureMonitorReplayMaxBytes: 1 << 20}
	for _, stream := range []string{"traces", "journal", "diagnostics"} {
		replay := cfg.azureReplayConfig(stream)
		if replay.start != start || replay.dir != filepath.Join(cfg.AzureMonitorReplayRoot, stream) {
			t.Fatalf("%s lost its caller-owned startup signal: %+v", stream, replay)
		}
	}
	cfg.AzureMonitorReplayRoot = ""
	if replay := cfg.azureReplayConfig("journal"); replay.dir != "" || replay.start != nil {
		t.Fatal("disabled replay acquired a startup signal")
	}
}

func TestAzureReplayStartGateRetainsAndReleases(t *testing.T) {
	start := make(chan struct{})
	sent := make(chan string, 8)
	s := startGateTestSpool(t, azureReplayConfig{dir: t.TempDir(), start: start}, func(_ context.Context, b []byte) error {
		sent <- string(b)
		return nil
	})
	payload := "{\"record\":\"before-ready\"}\n"
	if err := s.submit(t.Context(), []byte(payload)); err != nil {
		t.Fatal(err)
	}
	assertStartGateHeld(t, sent)
	stats := s.stats()
	if stats.Accepted != 1 {
		t.Fatalf("startup gate blocked recording: %+v", stats)
	}
	if stats.AccountingReady {
		if stats.PendingRecords != 1 {
			t.Fatalf("ready startup accounting omitted retained batch: %+v", stats)
		}
	} else {
		// The cold index may still be reconciling. An accepted pre-index
		// batch must be on disk even though manifest accounting is not ready.
		// A concurrent migration can rename it between listing and reading.
		found := false
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && !found; {
			for _, dir := range []string{bootstrapDir(s.index.root, s.stream), s.cfg.dir} {
				files, err := filepath.Glob(filepath.Join(dir, "*"+azureReplayFileSuffix))
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range files {
					if _, got, err := readAzureReplayFile(file); err == nil && string(got) == payload {
						found = true
					}
				}
			}
			if !found {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if !found {
			t.Fatalf("accepted startup batch was not durable; stats: %+v", stats)
		}
	}
	close(start)
	assertStartGateDelivered(t, sent, payload)
}

func TestAzureReplayStartGateCloseBeforeReadyRetainsForRestart(t *testing.T) {
	start := make(chan struct{})
	root := t.TempDir()
	cfg := azureReplayConfig{root: root, dir: filepath.Join(root, "journal"), start: start}
	sent := make(chan string, 8)
	offline := errors.New("test ingestion offline")
	s := startGateTestSpool(t, cfg, func(_ context.Context, b []byte) error {
		sent <- string(b)
		return offline
	})
	payload := "{\"record\":\"failed-startup\"}\n"
	if err := s.submit(t.Context(), []byte(payload)); err != nil {
		t.Fatal(err)
	}
	assertStartGateHeld(t, sent)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := s.close(ctx); err != nil && !errors.Is(err, offline) {
		t.Fatalf("close before ready = %v, want durable deferred replay or bounded final send", err)
	}
	// If indexing finished before close, a final send can fail offline. If
	// indexing was still deferred, close leaves the fsynced bootstrap batch for
	// restart instead. Neither path may silently discard the record.
	select {
	case got := <-sent:
		if got != payload {
			t.Fatalf("final send payload = %q, want %q", got, payload)
		}
	default:
	}
	select {
	case <-start:
		t.Fatal("closing one exporter released the caller's startup signal")
	default:
	}
	for name, done := range map[string]<-chan struct{}{"replay": s.done, "health": s.healthDone} {
		select {
		case <-done:
		default:
			t.Fatalf("%s worker still running after close", name)
		}
	}
	azureReplayIndexes.Lock()
	_, retained := azureReplayIndexes.roots[root]
	azureReplayIndexes.Unlock()
	if retained {
		t.Fatal("close before ready leaked shared index")
	}
	indexed, err := filepath.Glob(filepath.Join(root, "journal", "*"+azureReplayFileSuffix))
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := filepath.Glob(filepath.Join(bootstrapDir(root, "journal"), "*"+azureReplayFileSuffix))
	if err != nil {
		t.Fatal(err)
	}
	files := append(indexed, bootstrap...)
	if len(files) != 1 {
		t.Fatalf("failed startup retained %d durable batches, want 1", len(files))
	}
	if _, got, err := readAzureReplayFile(files[0]); err != nil || string(got) != payload {
		t.Fatalf("failed startup retained wrong batch: %q, %v", got, err)
	}
	if stats := InspectAzureReplayRoot(root); stats.AccountingReady && stats.PendingRecords != 1 {
		t.Fatalf("fully accounted failed startup omitted its durable record: %+v", stats)
	}
	cfg.start = nil
	_ = startGateTestSpool(t, cfg, func(_ context.Context, b []byte) error { sent <- string(b); return nil })
	assertStartGateDelivered(t, sent, payload)
}

func TestAzureReplayStartGateCancellationAndIsolation(t *testing.T) {
	start := make(chan struct{})
	heldSent := make(chan string, 8)
	held := startGateTestSpool(t, azureReplayConfig{dir: t.TempDir(), start: start}, func(_ context.Context, b []byte) error {
		heldSent <- string(b)
		return nil
	})
	freeSent := make(chan string, 8)
	free := startGateTestSpool(t, azureReplayConfig{dir: t.TempDir()}, func(_ context.Context, b []byte) error {
		freeSent <- string(b)
		return nil
	})
	payload := "{\"record\":\"isolated\"}\n"
	for _, s := range []*azureReplaySpool{held, free} {
		if err := s.submit(t.Context(), []byte(payload)); err != nil {
			t.Fatal(err)
		}
	}
	assertStartGateDelivered(t, freeSent, payload)
	if err := free.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertStartGateHeld(t, heldSent)
	held.cancel()
	select {
	case <-held.done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not stop gated replay worker")
	}
	close(start) // A signal after cancellation must not resurrect that worker.
	assertStartGateHeld(t, heldSent)
	if err := held.close(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertStartGateDelivered(t, heldSent, payload)
	// Exporters created after readiness must start without a second release.
	late := startGateTestSpool(t, azureReplayConfig{dir: t.TempDir(), start: start}, func(_ context.Context, b []byte) error {
		freeSent <- string(b)
		return nil
	})
	if err := late.submit(t.Context(), []byte(payload)); err != nil {
		t.Fatal(err)
	}
	assertStartGateDelivered(t, freeSent, payload)
}

func startGateTestSpool(t *testing.T, cfg azureReplayConfig, send func(context.Context, []byte) error) *azureReplaySpool {
	t.Helper()
	cfg.maxAge, cfg.maxBytes = time.Hour, 1<<20
	s, err := newAzureReplaySpool(cfg, send)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.close(ctx)
	})
	return s
}

func assertStartGateHeld(t *testing.T, sent <-chan string) {
	t.Helper()
	select {
	case payload := <-sent:
		t.Fatalf("background replay ran before its startup signal: %s", payload)
	case <-time.After(100 * time.Millisecond):
	}
}

func assertStartGateDelivered(t *testing.T, sent <-chan string, want string) {
	t.Helper()
	select {
	case got := <-sent:
		if got != want {
			t.Fatalf("replayed payload = %q, want %q", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("record did not replay")
	}
}
