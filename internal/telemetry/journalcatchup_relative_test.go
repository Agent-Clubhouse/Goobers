package telemetry

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// #6058: `goobers up .` produced a relative replay spool root. The cursor
// store opens SQLite through sqliteuri.File, which only accepts absolute
// paths, so a relative root resolved to the filesystem root and the store
// never opened: journal catch-up retried forever and no run journal exported.
func TestJournalCursorStoreOpensUnderRelativeRoot(t *testing.T) {
	t.Chdir(t.TempDir())
	db, _, err := openJournalCursorStore(context.Background(), filepath.Join("telemetry-export", "azure-monitor"), time.Now())
	if err != nil {
		t.Fatalf("openJournalCursorStore(relative root): %v", err)
	}
	_ = db.Close()
}
