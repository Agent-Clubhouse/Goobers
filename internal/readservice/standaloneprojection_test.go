package readservice

import (
	"io"
	"log"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/readmodel"
)

// TestStartStandaloneProjectionStopIsIdempotent pins the stop contract the
// standalone dashboard's close relies on: stop waits for the catch-up
// goroutine, and a second call returns instead of blocking or panicking.
func TestStartStandaloneProjectionStopIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	store, err := readmodel.Open(filepath.Join(dir, "read.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	stop := StartStandaloneProjection(store, instance.NewLayout(dir), log.New(io.Discard, "", 0))
	stop()
	stop()
}
