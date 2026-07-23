package credentials

import (
	"os"
	"testing"
)

func assertAskpassProtected(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat askpass helper: %v", err)
	}
}
