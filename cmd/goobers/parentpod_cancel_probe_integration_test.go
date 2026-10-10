//go:build integration

package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// This test-only endpoint proves that the generated shell actually began. It
// supplies no runtime authority and is never used as stopped-writer evidence.
func parentQualificationCancellationProbe(t *testing.T, repository string) <-chan struct{} {
	t.Helper()
	started := make(chan struct{})
	var once sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		once.Do(func() { close(started) })
	}))
	t.Cleanup(server.Close)
	endpoint := "http://host.docker.internal:" + strconv.Itoa(server.Listener.Addr().(*net.TCPAddr).Port) + "/started"
	if err := os.WriteFile(filepath.Join(repository, "qualification-notify"), []byte(endpoint), 0600); err != nil {
		t.Fatal(err)
	}
	return started
}
