//go:build integration

package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationRecoveryMirrorOfflineRetryAndRefresh(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	source := newSourceRepo(t)
	allowRemote := false
	m, err := NewManager(t.TempDir(), WithRemoteGitGate(func(context.Context, string) error {
		if !allowRemote {
			t.Fatal("recovery initialization attempted remote Git")
		}
		return nil
	}), WithGitEnvironment(func(context.Context, string) ([]string, error) {
		if !allowRemote {
			t.Fatal("recovery initialization resolved credentials")
		}
		return os.Environ(), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(m.Root, repoKey(source))
	staging := filepath.Join(parent, "recovery-mirror-init")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	// Simulate process death after init but before durable directory publication.
	runTestGit(t, staging, "init", "--bare")
	var mirror string
	visitErr := errors.New("intake refused")
	for range 2 {
		err := m.WithRecoveryMirror(ctx, source, func(dir string) error {
			mirror = dir
			if lock := m.lockFor(repoKey(source)); lock.TryLock() {
				lock.Unlock()
				t.Fatal("custody visitor ran without repository lock")
			}
			return visitErr
		})
		if !errors.Is(err, visitErr) {
			t.Fatalf("visitor error lost: %v", err)
		}
	}
	if _, err := os.Lstat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published staging directory remains: %v", err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 1 {
		t.Fatalf("retry grew repository directories: %v %v", entries, err)
	}
	if got := strings.TrimSpace(runTestGit(t, mirror, "config", "--get", "remote.origin.url")); got != source {
		t.Fatalf("origin identity: %q", got)
	}
	if refs := strings.TrimSpace(runTestGit(t, mirror, "for-each-ref")); refs != "" {
		t.Fatalf("offline initialization fetched refs: %s", refs)
	}
	allowRemote = true
	if got, err := m.WorkingCopy(ctx, source); err != nil || got != mirror {
		t.Fatalf("normal mirror refresh failed: %q %v", got, err)
	}
	if got := strings.TrimSpace(runTestGit(t, mirror, "rev-parse", "refs/heads/main")); got != strings.TrimSpace(runTestGit(t, source, "rev-parse", "HEAD")) {
		t.Fatal("normal refresh did not populate the initialized mirror")
	}
}
