package worktree

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPinnedCustodyRejectsInvalidMetadataBeforeHandoff(t *testing.T) {
	valid := marker{RunID: "owner", OwnerRunID: "owner", RepositoryDigest: RepositoryDigest("repo"), CreatedAt: time.Now().UTC()}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"valid", "oversize", "truncated", "empty", "foreign", "directory"} {
		t.Run(mode, func(t *testing.T) {
			m, err := NewManager(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			key := repoKey("repo")
			root := filepath.Join(m.pinnedRoot, key)
			if err := os.MkdirAll(filepath.Join(root, "pin"), 0o700); err != nil {
				t.Fatal(err)
			}
			body := string(data)
			switch mode {
			case "oversize":
				body += strings.Repeat(" ", 8193)
			case "truncated":
				body = body[:len(body)-1]
			case "empty":
				body = "{}"
			case "foreign":
				body = strings.Replace(body, `"owner_run_id":"owner"`, `"owner_run_id":"other"`, 1)
			}
			path := filepath.Join(root, pinnedCustodyFile)
			if mode == "directory" {
				err = os.Mkdir(path, 0o700)
			} else {
				err = os.WriteFile(path, []byte(body), 0o600)
			}
			if err != nil {
				t.Fatal(err)
			}
			called := false
			if err := m.SetCleanupGuard("recovery", func(context.Context, CleanupTarget) error { called = true; return nil }); err != nil {
				t.Fatal(err)
			}
			err = m.handoffPinnedState(context.Background(), key, "owner")
			if want := mode == "valid"; called != want || (err == nil) != want {
				t.Fatalf("handoff called=%t error=%v", called, err)
			}
		})
	}
}
