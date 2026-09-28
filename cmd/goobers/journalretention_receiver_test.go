package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

type retentionReplayReceiver struct {
	allow      atomic.Bool
	mu         sync.Mutex
	keys       map[string]string
	duplicates int
	invalid    bool
}

func newRetentionReplayReceiver(t *testing.T) (*retentionReplayReceiver, *httptest.Server) {
	t.Helper()
	receiver := &retentionReplayReceiver{keys: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(receiver.serveHTTP))
	t.Cleanup(server.Close)
	return receiver, server
}

func (r *retentionReplayReceiver) serveHTTP(w http.ResponseWriter, request *http.Request) {
	if !r.allow.Load() {
		_, _ = io.Copy(io.Discard, request.Body)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	decoded, err := gzip.NewReader(request.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	defer func() { _ = decoded.Close() }()
	scanner := bufio.NewScanner(decoded)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	for scanner.Scan() {
		var envelope struct {
			Data struct {
				BaseData struct{ Properties map[string]string }
			}
		}
		if json.Unmarshal(scanner.Bytes(), &envelope) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p := envelope.Data.BaseData.Properties
		if p["goobers.journal.kind"] != "run" {
			continue
		}
		key, id := p["goobers.run.id"]+":"+p["goobers.journal.seq"], p["goobers.telemetry.record_id"]
		r.mu.Lock()
		if prior, seen := r.keys[key]; seen {
			r.duplicates++
			r.invalid = r.invalid || prior != id
		}
		r.invalid = r.invalid || len(id) != 64 || p["goobers.run.id"] == "" || p["goobers.journal.seq"] == ""
		r.keys[key] = id
		r.mu.Unlock()
	}
	if scanner.Err() != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (r *retentionReplayReceiver) hasKeys(wanted map[string]string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range wanted {
		if _, ok := r.keys[key]; !ok {
			return false
		}
	}
	return true
}

func (r *retentionReplayReceiver) verify(t *testing.T, wanted map[string]string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.invalid || len(r.keys) != len(wanted) {
		t.Fatalf("replay keys=%d want=%d invalid=%v", len(r.keys), len(wanted), r.invalid)
	}
	for key := range wanted {
		if _, ok := r.keys[key]; !ok {
			t.Errorf("missing pre-prune source key %s", key)
		}
	}
	t.Logf("received stable replay identities=%d duplicates=%d", len(r.keys), r.duplicates)
}

func obstructRetentionReplay(t *testing.T, spool string) func() {
	t.Helper()
	dir := filepath.Join(spool, "journal")
	backup := filepath.Join(spool, "journal-before-test-obstruction")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("expected empty owned replay directory: entries=%d err=%v", len(entries), err)
	}
	if err := os.Rename(dir, backup); err != nil {
		t.Fatal(err)
	}
	marker := []byte("synthetic retention admission obstruction\n")
	if err := os.WriteFile(dir, marker, 0600); err != nil {
		t.Fatal(err)
	}
	return func() {
		info, err := os.Lstat(dir)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("owned obstruction changed type: %v", err)
		}
		body, err := os.ReadFile(dir)
		if err != nil || string(body) != string(marker) {
			t.Fatalf("owned obstruction changed contents: %v", err)
		}
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(backup, dir); err != nil {
			t.Fatal(err)
		}
	}
}
