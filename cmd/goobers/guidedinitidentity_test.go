package main

import (
	"os"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

type guidedInitIdentityTestWriter struct {
	t    *testing.T
	root string
}

func (w guidedInitIdentityTestWriter) Write(data []byte) (int, error) {
	w.t.Helper()
	id, err := instance.ReadRootIdentity(w.root)
	if err != nil || !strings.Contains(string(data), id) || !strings.Contains(string(data), canonicalStatusRoot(w.root)) {
		w.t.Fatalf("guided target not identified: %q %v", data, err)
	}
	if _, err := os.Stat(instance.NewLayout(w.root).ConfigFile()); !os.IsNotExist(err) {
		w.t.Fatalf("guided config written before target display: %v", err)
	}
	return len(data), nil
}
