package recovery

import (
	"os"
	"path/filepath"
	"testing"
)

// Only a checkout with nothing left to read is settled as gone (#6940); a
// live gitdir link or any remaining file keeps the ordinary capture path.
func TestGoneCheckoutState(t *testing.T) {
	root := t.TempDir()
	admin := filepath.Join(root, "admin")
	if err := os.Mkdir(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{name: "empty", files: map[string]string{}, want: "empty"},
		{name: "dangling-gitdir", files: map[string]string{".git": "gitdir: " + filepath.Join(root, "pruned") + "\n"}, want: "empty"},
		{name: "live-gitdir", files: map[string]string{".git": "gitdir: " + admin + "\n"}},
		{name: "malformed-git-file", files: map[string]string{".git": "not a gitdir link\n"}},
		{name: "files-without-git", files: map[string]string{"work.txt": "unsaved\n"}},
		{name: "dangling-gitdir-with-files", files: map[string]string{".git": "gitdir: " + filepath.Join(root, "pruned") + "\n", "work.txt": "unsaved\n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name)
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			for name, content := range tc.files {
				if err := os.WriteFile(filepath.Join(path, name), []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := goneCheckoutState(path); err != nil || got != tc.want {
				t.Fatalf("goneCheckoutState() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	if got, err := goneCheckoutState(filepath.Join(root, "absent")); err != nil || got != "missing" {
		t.Fatalf("goneCheckoutState(absent) = %q, %v; want missing", got, err)
	}
}
