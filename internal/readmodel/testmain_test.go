package readmodel

import (
	"os"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// TestMain disables journal fsync for this suite (#3574/#4127), the same seam
// `make ci` applies through JOURNAL_TEST_FSYNC_OFF and cmd/goobers applies from
// its own TestMain (#827).
//
// This package's fixtures write real run journals and real SQLite stores —
// hundreds of them, one per fixture run — and fsync is the one syscall whose
// cost is set by the host rather than by the work. On the Windows runner that
// cost is high enough to change the answer: the suite ran to a "panic: test
// timed out after 10m0s" whose goroutine dump caught the process inside
// windows.FlushFileBuffers, having spent the whole default `go test` budget on
// durability that a t.TempDir fixture deleted moments later. Measured on a
// developer machine, where fsync is cheap by comparison, the same suites still
// cost 2-4x more with it on.
//
// The seam belongs here rather than in the workflow that ran into it: a caller
// that forgets the env — a bare `go test ./internal/readmodel`, a new CI step,
// the windows gate that ran these packages raw — would otherwise be slow again
// with nothing to say so. Test instances are ephemeral scratch with no
// durability requirement, and production never sets this.
//
// Tests that need the real setting (inventory reports it) override it with
// t.Setenv, which restores the process value afterwards.
func TestMain(m *testing.M) {
	if err := os.Setenv("GOOBERS_DISABLE_FSYNC", "1"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// TestJournalFsyncDisabledForSuite is the recurrence guard: if the TestMain
// above is dropped, this goes red instead of the suite quietly growing back
// toward the ten-minute package timeout it already hit once.
func TestJournalFsyncDisabledForSuite(t *testing.T) {
	if !journal.FsyncDisabled() {
		t.Fatal("journal fsync is enabled for this suite; the #3574/#4127 fsync-disable seam " +
			"(TestMain) is not in effect, and this package's journal-writing fixtures will " +
			"spend the whole `go test` budget on durability no test needs")
	}
}
