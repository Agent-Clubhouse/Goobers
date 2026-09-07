package repair

import (
	"os"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

// TestMain disables journal fsync for this suite. Same seam, same reason, and
// the same reported failure as internal/readmodel's TestMain, whose comment
// carries the rationale in full (#3574/#4127): this package's fixtures write
// real run journals and SQLite stores into t.TempDir scratch, and on the
// Windows runner the durability those writes ask for cost more than the whole
// default `go test` budget.
//
// Tests that need the real setting override it with t.Setenv.
func TestMain(m *testing.M) {
	if err := os.Setenv("GOOBERS_DISABLE_FSYNC", "1"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// TestJournalFsyncDisabledForSuite is the recurrence guard: dropping the
// TestMain above goes red here rather than growing the suite back toward the
// package timeout it already hit.
func TestJournalFsyncDisabledForSuite(t *testing.T) {
	if !journal.FsyncDisabled() {
		t.Fatal("journal fsync is enabled for this suite; the #3574/#4127 fsync-disable seam " +
			"(TestMain) is not in effect")
	}
}
