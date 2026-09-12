package instance

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestInitIdentityObserverPrecedesScaffoldAndRetryKeepsID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "new-root")
	var first string
	_, err := Init(root, func(path, id string) error {
		first = id
		if path != root || id == "" {
			t.Fatalf("wrong initialization target: %q %q", path, id)
		}
		for _, child := range []string{ConfigFileName, ConfigDirName, "scheduler", "gaggles", TelemetryDBName} {
			if _, err := os.Stat(filepath.Join(root, child)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("%s written before identity observation: %v", child, err)
			}
		}
		return io.ErrClosedPipe
	})
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("failed observer ignored: %v", err)
	}
	if _, err := os.Stat(NewLayout(root).ConfigFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scaffold continued after failed observer: %v", err)
	}
	if _, err := Init(root, func(_ string, id string) error {
		if id != first {
			t.Fatalf("retry changed durable identity: %s != %s", id, first)
		}
		return nil
	}); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
}
