package worktree

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoveryMarkerGaggleBoundOnWriteAndRead(t *testing.T) {
	for _, trigger := range []string{strings.Repeat("x", 1025), "issue:42\nother", "issue:42\x00"} {
		filename := filepath.Join(t.TempDir(), "marker.json")
		value := marker{Gaggle: trigger}
		if err := writeMarker(filename, value); err == nil {
			t.Fatal("invalid trigger was persisted")
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, data, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := readMarker(filename); err == nil {
			t.Fatal("invalid stored trigger reached cleanup")
		}
	}
}
