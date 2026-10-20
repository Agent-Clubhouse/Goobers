package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReadBoundaryRejectsWritableAncestorsAndEscapingExceptions(t *testing.T) {
	root := t.TempDir()
	own := filepath.Join(root, "own")
	if err := os.Mkdir(own, 0700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		boundary  ReadBoundary
		workspace string
	}{
		{"entire-root", ReadBoundary{Root: root, Allow: []string{root}}, own},
		{"outside", ReadBoundary{Root: root, Allow: []string{t.TempDir()}}, own},
		{"writable-ancestor", ReadBoundary{Root: root, Allow: []string{own}}, root},
		{"missing-exception", ReadBoundary{Root: root}, own},
		{"missing-root", ReadBoundary{Root: filepath.Join(root, "missing")}, own},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := validate(exec.Command("/bin/true"), Policy{Workspace: test.workspace, ReadBoundaries: []ReadBoundary{test.boundary}})
			if err == nil {
				t.Fatal("unsafe boundary accepted")
			}
		})
	}
	if _, err := validate(exec.Command("/bin/true"), Policy{Workspace: own, ReadBoundaries: []ReadBoundary{{Root: root, Allow: []string{own}}}}); err != nil {
		t.Fatal(err)
	}
}
