package configmirror

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPlanePublishesDigestAndRetainsThreeSupersededSnapshots(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	writeTestFile(t, filepath.Join(config, "manifest.yaml"), "generation: 0")
	plane, err := NewPlane(filepath.Join(root, "plane"))
	if err != nil {
		t.Fatal(err)
	}
	var digests []string
	for generation := range 5 {
		writeTestFile(t, filepath.Join(config, "manifest.yaml"), fmt.Sprintf("generation: %d", generation))
		digest, err := plane.PublishValidated(t.Context(), config, []byte("instance"), func(string) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		digests = append(digests, digest)
		if plane.Current() != digest {
			t.Fatalf("current = %q, want %q", plane.Current(), digest)
		}
	}
	if _, _, err := plane.Open(digests[0]); !os.IsNotExist(err) {
		t.Fatalf("oldest snapshot retained: %v", err)
	}
	for _, digest := range digests[1:] {
		file, _, err := plane.Open(digest)
		if err != nil {
			t.Fatalf("open retained %s: %v", digest, err)
		}
		if _, err := io.Copy(io.Discard, file); err != nil {
			t.Fatal(err)
		}
		_ = file.Close()
	}
	reopened, err := NewPlane(plane.Root)
	if err != nil || reopened.Current() != digests[len(digests)-1] {
		t.Fatalf("reopen current = %q, %v", reopened.Current(), err)
	}
}
