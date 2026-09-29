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
	if stats := s.stats(); !stats.AccountingReady || stats.PendingRecords != 1 || stats.Accepted != 1 {
		t.Fatalf("startup gate blocked accounting or recording: %+v", stats)
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
	if err := s.close(ctx); !errors.Is(err, offline) {
		t.Fatalf("close before ready = %v, want bounded final send's offline error", err)
	}
	assertStartGateDelivered(t, sent, payload) // Close bypasses the startup gate.
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
	if stats := InspectAzureReplayRoot(root); !stats.AccountingReady || stats.PendingRecords != 1 {
		t.Fatalf("failed startup lost its durable record: %+v", stats)
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
