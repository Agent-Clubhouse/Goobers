package configmirror

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestOpenedSnapshotPinsCompleteGeneration(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	mirror := filepath.Join(root, "mirror")
	instructions := strings.Repeat("worker instructions\n", 100000) // Beyond ConfigMap's ceiling.
	writeTestFile(t, filepath.Join(config, "gaggles/web/instructions.md"), instructions)
	writeTestFile(t, filepath.Join(root, "credentials/token"), "must not be copied")
	if err := Publish(t.Context(), mirror, config, []byte("generation: first\n")); err != nil {
		t.Fatal(err)
	}
	first, err := Open(mirror)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	writeTestFile(t, filepath.Join(config, "gaggles/web/instructions.md"), "second")
	if err := Publish(t.Context(), mirror, config, []byte("generation: second\n")); err != nil {
		t.Fatal(err)
	}
	second, err := Open(mirror)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	for _, test := range []struct {
		s                        *Snapshot
		generation, instructions string
	}{{first, "first", instructions}, {second, "second", "second"}} {
		dest := t.TempDir()
		if err := test.s.Extract(t.Context(), dest); err != nil {
			t.Fatal(err)
		}
		doc, err := os.ReadFile(filepath.Join(dest, "instance.yaml"))
		if err != nil || string(doc) != "generation: "+test.generation+"\n" {
			t.Fatalf("document=%q error=%v", doc, err)
		}
		body, err := os.ReadFile(filepath.Join(dest, "config/gaggles/web/instructions.md"))
		if err != nil || string(body) != test.instructions {
			t.Fatalf("mixed instructions: length=%d error=%v", len(body), err)
		}
		if _, err := os.Stat(filepath.Join(dest, "credentials")); !os.IsNotExist(err) {
			t.Fatalf("credential files copied: %v", err)
		}
	}
}

func TestRejectedPublicationPreservesPreviousSnapshot(t *testing.T) {
	root := t.TempDir()
	config, mirror := filepath.Join(root, "config"), filepath.Join(root, "mirror")
	writeTestFile(t, filepath.Join(config, "instructions.md"), "original")
	if err := Publish(t.Context(), mirror, config, []byte("instance")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(config, "oversized"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Publish(t.Context(), mirror, config, []byte("replacement")); err == nil {
		t.Fatal("published oversized snapshot")
	}
	s, err := Open(mirror)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	dest := t.TempDir()
	if err := s.Extract(t.Context(), dest); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dest, "instance.yaml"))
	if err != nil || string(data) != "instance" {
		t.Fatalf("previous publication lost: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(mirror, ".worker-config.pending")); !os.IsNotExist(err) {
		t.Fatalf("staging leak: %v", err)
	}
}

func TestExtractRejectsTraversalLinksAndCaseCollisions(t *testing.T) {
	for _, names := range [][]string{{"../escape"}, {"config/../../escape"}, {"config\\escape"}, {"config/C:escape"}, {"config/A", "config/a"}, {"config/NUL.txt"}, {"config/trailing."}, {"link"}} {
		t.Run(strings.Join(names, ","), func(t *testing.T) {
			mirror := t.TempDir()
			f, err := os.Create(filepath.Join(mirror, SnapshotName))
			if err != nil {
				t.Fatal(err)
			}
			z := zip.NewWriter(f)
			if err := writeEntry(z, "instance.yaml", strings.NewReader("instance")); err != nil {
				t.Fatal(err)
			}
			for _, name := range names {
				header := zip.FileHeader{Name: name, Method: zip.Store}
				if name == "link" {
					header.Name = "config/link"
					header.SetMode(os.ModeSymlink | 0o777)
				}
				w, err := z.CreateHeader(&header)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write([]byte("payload")); err != nil {
					t.Fatal(err)
				}
			}
			if err := z.Close(); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			s, err := Open(mirror)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			if err := s.Extract(t.Context(), t.TempDir()); err == nil {
				t.Fatal("accepted unsafe snapshot")
			}
		})
	}
}
