//go:build !windows

package credentials

import (
	"os"
	"testing"
)

func assertAskpassProtected(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat askpass script: %v", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("askpass script perms = %v, want no group/other access", info.Mode().Perm())
	}
}
