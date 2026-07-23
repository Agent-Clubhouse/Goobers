package credentials

import (
	"context"
	"os/exec"
	"os/user"
	"path/filepath"
	"testing"
)

func TestResolverResolvesFromOwnerOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	writeFile(t, path, "file-secret\n")
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(
		"icacls.exe",
		path,
		"/inheritance:r",
		"/grant:r",
		current.Username+":F",
	).CombinedOutput(); err != nil {
		t.Fatalf("restrict token ACL: %v\n%s", err, output)
	}
	resolver, err := NewResolver([]TokenRef{{Name: "gh", File: path}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolver.Resolve(context.Background(), "gh")
	if err != nil {
		t.Fatal(err)
	}
	if got != "file-secret" {
		t.Fatalf("Resolve = %q, want file-secret", got)
	}
}
