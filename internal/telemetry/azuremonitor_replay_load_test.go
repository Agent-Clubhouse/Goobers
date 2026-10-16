package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAzureReplaySlowUploadDoesNotBlockAdmissionOrOtherStreams(t *testing.T) {
	root := t.TempDir()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s, err := newAzureReplaySpool(azureReplayConfig{
		root: root, dir: filepath.Join(root, "journal"), maxAge: time.Hour, maxBytes: 1 << 20,
	}, func(ctx context.Context, _ []byte) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		closeReplaySpoolForTest(t, s)
	})
	if err := s.submit(context.Background(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upload never started")
	}
	other := testAzureReplaySpool(t, filepath.Join(root, "diagnostics"), time.Now())
	other.cfg.root = root
	// Acquire the shared manifest now so other's cleanup releases it; a lazy
	// acquisition after cleanup would leak an open handle (#6683).
	if err := other.ensureIndex(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var producer sync.WaitGroup
	producer.Add(1)
	// Registered last, so it runs first: no admission may outlive the spools.
	t.Cleanup(func() {
		unblock()
		producer.Wait()
	})
	go func() {
		defer producer.Done()
		if err := s.submit(context.Background(), []byte("{}\n")); err != nil {
			done <- err
			return
		}
		if err := other.submit(context.Background(), []byte("{}\n")); err != nil {
			done <- err
			return
		}
		_ = s.stats()
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		// The upload stays blocked until cleanup, so a regression never
		// completes; the generous bound only tolerates slow CI filesystems.
		t.Fatal("slow HTTP held up disk admission or stats")
	}
}

func TestAzureReplayContinuousAdmissionHonorsRetryBackoff(t *testing.T) {
	var calls atomic.Int32
	var mu sync.Mutex
	var attempts []time.Time
	s, err := newAzureReplaySpool(azureReplayConfig{
		dir: t.TempDir(), maxAge: time.Hour, maxBytes: 1 << 20,
	}, func(context.Context, []byte) error {
		mu.Lock()
		attempts = append(attempts, time.Now())
		mu.Unlock()
		calls.Add(1)
		return errors.New("offline")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeReplaySpoolForTest(t, s) })
	if err := s.submit(context.Background(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("initial attempt missing")
	}
	start := time.Now()
	for time.Since(start) < 300*time.Millisecond {
		if err := s.submit(context.Background(), []byte("{}\n")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	deadline = time.Now().Add(3 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if calls.Load() < 2 {
		t.Fatal("retry did not resume after backoff")
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(attempts); i++ {
		if gap := attempts[i].Sub(attempts[i-1]); gap < azureReplayRetryMinimum {
			t.Fatalf("new arrivals bypassed backoff: attempt interval=%s", gap)
		}
	}
}

// closeReplaySpoolForTest closes s with a short deadline, then joins the
// deadline fallback so t.TempDir cleanup cannot race an open manifest handle.
func closeReplaySpoolForTest(t testing.TB, s *azureReplaySpool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.close(ctx)
	<-s.released
}

// #6684/#6683: close must not release the manifest while a worker can still
// query it, and once released, Windows must allow the file to be renamed and
// removed. The health worker is parked mid-report, so a past-deadline close
// deterministically takes its fallback; releasing early fails the first check.
func TestAzureReplayCloseReleasesIndexHandle(t *testing.T) {
	for range 8 {
		root := t.TempDir()
		s, err := newAzureReplaySpool(azureReplayConfig{dir: root, maxAge: time.Hour, maxBytes: 1 << 20},
			func(context.Context, []byte) error { return errors.New("offline") })
		if err != nil {
			t.Fatal(err)
		}
		if err := s.submit(context.Background(), []byte("{}\n")); err != nil {
			t.Fatal(err)
		}
		entered, gate := make(chan struct{}), make(chan struct{})
		var parked atomic.Bool
		setReplayLossSource(&s.lossSource, func() replayLossCounters {
			if parked.CompareAndSwap(false, true) {
				close(entered)
				<-gate
			}
			return replayLossCounters{}
		})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.close(ctx); !errors.Is(err, context.Canceled) {
			close(gate)
			t.Fatalf("close with a parked health worker = %v, want its deadline fallback", err)
		}
		<-entered
		select {
		case <-s.released:
			close(gate)
			t.Fatal("index released while the health worker may still query it")
		default:
		}
		close(gate)
		<-s.released
		path := filepath.Join(root, azureReplayIndexName)
		if err := os.Rename(path, path+".moved"); err != nil {
			t.Fatalf("manifest still open after close: %v", err)
		}
		if err := os.Remove(path + ".moved"); err != nil {
			t.Fatalf("manifest still open after close: %v", err)
		}
	}
}

// An initialized manifest closes before release returns, even past the
// caller's deadline; a random select once deferred it to a goroutine (#6684).
func TestAzureReplayIndexReleasePastDeadlineClosesManifest(t *testing.T) {
	for range 8 {
		root := t.TempDir()
		x, _, err := acquireReplayIndex(azureReplayConfig{dir: root})
		if err != nil {
			t.Fatal(err)
		}
		if err := x.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := x.release(ctx); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, azureReplayIndexName)
		if err := os.Rename(path, path+".moved"); err != nil {
			t.Fatalf("manifest still open after release: %v", err)
		}
	}
}

func TestAzureReplayDrainWorkIsBounded(t *testing.T) {
	s := testAzureReplaySpool(t, t.TempDir(), time.Now())
	const count = azureReplayDrainLimit + 5
	for range count {
		if err := s.submit(context.Background(), []byte(strings.Repeat("{}\n", azureReplayBatchRecords))); err != nil {
			t.Fatal(err)
		}
	}
	s.send = func(context.Context, []byte) error { return nil }
	if err := s.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := indexedReplayWorkStats(t, s)
	if stats.Delivered != azureReplayDrainLimit*azureReplayBatchRecords || stats.PendingRecords != 5*azureReplayBatchRecords {
		t.Fatalf("unbounded drain: %+v", stats)
	}
	if err := s.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if stats := indexedReplayWorkStats(t, s); stats.Delivered != count*azureReplayBatchRecords || stats.PendingRecords != 0 {
		t.Fatalf("incomplete continuation: %+v", stats)
	}
}

// This test is about the drainer's work budget, not the best-effort 100 ms
// live health snapshot. A race-instrumented, shared CI host can miss that
// short read deadline despite a successful durable drain; give the manifest
// query its own bounded deadline and require authoritative accounting.
func indexedReplayWorkStats(t *testing.T, s *azureReplaySpool) AzureReplayStats {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stats, err := s.index.stats(ctx, s.stream, s.now())
	if err != nil || !stats.AccountingReady {
		t.Fatalf("drain accounting unavailable: stats=%+v err=%v", stats, err)
	}
	stats.Delivered = s.delivered.Load()
	return stats
}

func TestAzureReplayBoundsKeepSendingBatchCharged(t *testing.T) {
	s := testAzureReplaySpool(t, t.TempDir(), time.Now())
	if err := s.submit(context.Background(), []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	files, err := s.filesLockedForTest()
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%v err=%v", files, err)
	}
	s.cfg.maxBytes = files[0].size + 8
	started, release := make(chan struct{}), make(chan struct{})
	s.send = func(context.Context, []byte) error { close(started); <-release; return nil }
	done := make(chan error, 1)
	go func() { done <- s.drain(context.Background()) }()
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("upload did not start")
	}
	if err := s.submit(context.Background(), []byte("{}\n")); err == nil {
		t.Fatal("admitted a new batch past the cap while old batch is sending")
	}
	stats := s.stats()
	if stats.PrunedBytes != 1 || stats.PendingRecords != 1 || stats.PendingBytes > s.cfg.maxBytes {
		t.Fatalf("stats=%+v", stats)
	}
	remaining, err := s.filesLockedForTest()
	if err != nil || len(remaining) != 1 || remaining[0].path != files[0].path {
		t.Fatalf("pruned in-flight file: remaining=%v err=%v", remaining, err)
	}
}

// Measures warm incremental admission, excluding one-time reconciliation.
// No network or worker is involved; production admission still fsyncs each file.
func BenchmarkAzureReplayBacklogAdmission(b *testing.B) {
	for _, files := range []int{0, 32, 256} {
		b.Run(fmt.Sprintf("files=%d", files), func(b *testing.B) {
			s := testAzureReplaySpool(b, b.TempDir(), time.Now())
			s.cfg.maxBytes = 1 << 30
			payload := []byte(strings.Repeat(`{"padding":"`+strings.Repeat("x", 1000)+"\"}\n", 128))
			for i := range files {
				if _, err := s.writeLocked(fmt.Sprintf("%020d-seed.ndjson", i), s.now(), payload); err != nil {
					b.Fatal(err)
				}
			}
			if err := s.ensureIndex(); err != nil {
				b.Fatal(err)
			}
			if err := s.index.wait(context.Background()); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := s.submit(context.Background(), payload); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(files*len(payload)), "initial-backlog-B")
		})
	}
}
