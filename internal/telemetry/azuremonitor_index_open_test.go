package telemetry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Separate manifest reconstruction from reopening a current manifest. This is
// not a daemon startup gate or a cold-OS-cache benchmark. Seed publication,
// fsync, priming, database close and correctness checks are outside timing.
func BenchmarkAzureReplayIndexOpen(b *testing.B) {
	for _, count := range []int{0, 12000} {
		for _, warm := range []bool{false, true} {
			b.Run(fmt.Sprintf("files=%d/warm=%t", count, warm), func(b *testing.B) {
				fixture := newReplayIndexOpenFixture(b, count)
				b.ReportAllocs()
				b.ResetTimer()
				b.StopTimer()
				for range b.N {
					audited := fixture.prepare(b, warm)
					index := fixture.index()
					b.StartTimer()
					err := index.open(b.Context())
					b.StopTimer()
					if err != nil {
						_ = index.closeDatabases()
						b.Fatal(err)
					}
					fixture.checkAndClose(b, index, audited)
				}
				b.ReportMetric(float64(count), "files/op")
			})
		}
	}
}

type replayIndexOpenFixture struct {
	root         string
	files        int
	payloadBytes int64
}

func newReplayIndexOpenFixture(t testing.TB, count int) replayIndexOpenFixture {
	t.Helper()
	f := replayIndexOpenFixture{root: t.TempDir(), files: count}
	dir := filepath.Join(f.root, "journal")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Use production publication/fsync but no exporter/index/background worker.
	spool := &azureReplaySpool{cfg: azureReplayConfig{dir: dir}}
	created := time.Now().UTC()
	for i := range count {
		path, err := spool.writeLocked(fmt.Sprintf("%020d-seed.ndjson", i), created, []byte("{}\n"))
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		f.payloadBytes += info.Size()
	}
	return f
}

func (f replayIndexOpenFixture) index() *azureReplayIndex {
	return &azureReplayIndex{root: f.root, streams: []string{"traces", "journal", "diagnostics"}}
}

func (f replayIndexOpenFixture) prepare(t testing.TB, warm bool) int64 {
	t.Helper()
	if !warm {
		// Only known manifest files in this newly allocated private fixture are
		// reset; authoritative replay payloads are retained between iterations.
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(filepath.Join(f.root, azureReplayIndexName+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
		}
		return 0
	}
	index := f.index()
	if err := index.open(t.Context()); err != nil {
		_ = index.closeDatabases()
		t.Fatal(err)
	}
	// Keep the benchmark's warm condition explicit even during long runs.
	// Expired-manifest audit cost is covered by BenchmarkAzureReplayIndexAudit.
	audited := time.Now().UnixNano()
	if _, err := index.db.ExecContext(t.Context(), `UPDATE reconciliation SET audited=?`, audited); err != nil {
		_ = index.closeDatabases()
		t.Fatal(err)
	}
	f.checkAndClose(t, index, audited)
	return audited
}

func (f replayIndexOpenFixture) checkAndClose(t testing.TB, index *azureReplayIndex, warmAudited int64) {
	t.Helper()
	defer func() {
		if err := index.closeDatabases(); err != nil {
			t.Error(err)
		}
	}()
	stats, err := replayIndexStats(t.Context(), index.statsDB, "*", time.Now())
	if err != nil || !stats.AccountingReady || stats.PendingFiles != f.files || stats.PendingRecords != f.files || stats.PendingBytes != f.payloadBytes {
		t.Fatalf("manifest disagrees with independently seeded files: stats=%+v err=%v", stats, err)
	}
	var audited int64
	if err := index.db.QueryRowContext(t.Context(), `SELECT audited FROM reconciliation WHERE id=1`).Scan(&audited); err != nil {
		t.Fatal(err)
	}
	if audited <= 0 || (warmAudited != 0 && audited != warmAudited) {
		t.Fatalf("manifest audit state changed unexpectedly: got=%d warm=%d", audited, warmAudited)
	}
}

func TestAzureReplayIndexOpenFixturePreservesPayloads(t *testing.T) {
	f := newReplayIndexOpenFixture(t, 3)
	for _, warm := range []bool{false, true, false, true} {
		audited := f.prepare(t, warm)
		_, err := os.Stat(filepath.Join(f.root, azureReplayIndexName))
		if (!warm && !errors.Is(err, os.ErrNotExist)) || (warm && err != nil) {
			t.Fatalf("wrong manifest precondition warm=%t: %v", warm, err)
		}
		index := f.index()
		if err := index.open(t.Context()); err != nil {
			_ = index.closeDatabases()
			t.Fatal(err)
		}
		f.checkAndClose(t, index, audited)
		for i := range f.files {
			_, payload, err := readAzureReplayFile(filepath.Join(f.root, "journal", fmt.Sprintf("%020d-seed.ndjson", i)))
			if err != nil || string(payload) != "{}\n" {
				t.Fatalf("authoritative seed changed: %q %v", payload, err)
			}
		}
	}
}
