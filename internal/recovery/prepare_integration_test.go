//go:build integration

package recovery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationPreparationPreservesEarlierStageCommitsAndDirtyWork(t *testing.T) {
	testdep.Require(t, "git")
	ctx := context.Background()
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	base := recoveryTestGit(t, repository, "rev-parse", "HEAD")
	recoveryTestGit(t, repository, "checkout", "-b", "implementation")
	for _, path := range []string{"earlier-stage.txt", "current-dirty.txt"} {
		if err := os.WriteFile(filepath.Join(repository, path), []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
		if path == "earlier-stage.txt" {
			recoveryTestGit(t, repository, "add", path)
			recoveryTestGit(t, repository, "commit", "-m", "earlier stage implementation")
		}
	}
	template := storageTestRecord()
	record, err := PrepareRecord(ctx, repository, template.RepositoryKey, template.RunID, "main", template.CreatedAt, template.RetainUntil)
	if err != nil {
		t.Fatal(err)
	}
	if record.BaseSHA != base {
		t.Fatal("preparation used the stage start instead of cumulative base")
	}
	if record.BaseRef != "refs/heads/main" {
		t.Fatalf("preparation base ref = %q, want refs/heads/main", record.BaseRef)
	}
	inventory := t.TempDir()
	published, recordPath, err := PublishToInventoryWithEviction(ctx, repository, inventory, []string{repository}, record, 1, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if retry, path, err := PublishToInventoryWithEviction(ctx, repository, inventory, []string{repository}, record, 1, 1<<20, nil); err != nil || retry != published || path != recordPath {
		t.Fatalf("full inventory rejected identical publication retry: %+v %q %v", retry, path, err)
	}
	commit, err := RestoreSnapshot(ctx, repository, published, base, "restored", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"earlier-stage.txt", "current-dirty.txt"} {
		if got := recoveryTestGit(t, repository, "show", commit+":"+path); got != path {
			t.Fatalf("lost implementation path %s: %s", path, got)
		}
	}
	retry, err := PrepareRecord(ctx, repository, template.RepositoryKey, template.RunID, "main", template.CreatedAt, template.RetainUntil)
	if err != nil || retry != record {
		t.Fatalf("identical preparation retry changed identity: %+v %v", retry, err)
	}
}

func TestIntegrationPreparationReportsMissingAttemptedBaseRef(t *testing.T) {
	testdep.Require(t, "git")
	repository := t.TempDir()
	recoveryTestGit(t, repository, "init", "--initial-branch=main")
	recoveryTestGit(t, repository, "commit", "--allow-empty", "-m", "base")
	template := storageTestRecord()
	const missing = "refs/heads/missing-base"
	_, err := PrepareRecord(context.Background(), repository, template.RepositoryKey, template.RunID, missing, template.CreatedAt, template.RetainUntil)
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing base diagnostic = %v; want attempted ref %q", err, missing)
	}
	if got := recoveryTestGit(t, repository, "status", "--porcelain"); got != "" {
		t.Fatalf("failed preparation changed source: %s", got)
	}
}
