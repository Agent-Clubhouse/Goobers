//go:build !windows

package instance

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGitSourceExistingRelativePathWithColonIsLocal(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "fixture:repo")
	if err := os.Mkdir(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	runGitSourceTest(t, repository, "init", "--initial-branch=main")
	t.Chdir(root)

	source, err := NewGitSource(GitSourceOptions{
		InstanceRoot: t.TempDir(),
		Repository:   "fixture:repo",
	})
	if err != nil {
		t.Fatalf("NewGitSource: %v", err)
	}
	if !source.local || source.mirror != "" {
		t.Fatalf("source = local %v, mirror %q; want existing colon-containing path treated as local", source.local, source.mirror)
	}
}
