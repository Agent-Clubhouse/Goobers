//go:build integration

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
)

func verifyParentOverflowCleanup(t *testing.T, reader *journal.Reader, restorer parentArchiveRestorer, writer *journal.Run, checkout *worktree.Worktree) {
	t.Helper()
	policy := restorer.config.Retention.RecoveryEffective()
	policy.MaxSnapshots = 1
	restorer.config.Retention.Recovery = &policy
	if err := instance.WriteConfig(restorer.layout.ConfigFile(), restorer.config); err != nil {
		t.Fatal(err)
	}
	// A fresh interrupted publisher still owns its reservation for the normal
	// recovery grace period. It keeps this one-slot inventory full even if
	// capacity reclamation retires older snapshots from the completed journey.
	reservation := filepath.Join(restorer.layout.Root, "recovery", strings.Repeat("b", 64))
	if err := os.Mkdir(reservation, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout.Path, "source.txt"), []byte("overflow dirty\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restorer.retire(writer); err != nil {
		t.Fatal("full inventory blocked parent retirement", err)
	}
	archive, seq := latestParentArchive(t, reader)
	data, err := reader.ArtifactBytesBounded(archive.Archive, 16384)
	if err != nil {
		t.Fatal(err)
	}
	var record recovery.Record
	if err := json.Unmarshal(data, &record); err != nil || record.ArchiveDigest != "" {
		t.Fatal("retirement did not declare mirror-only durability", record, err)
	}
	id, err := reader.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeTerminalRunWithClaimRelease(restorer.layout, nil, restorer.worktrees, id.RunID, func(instance.Layout, *journal.InstanceLog, string) error { return nil }); err != nil {
		t.Fatal("overflow parent finalization", err)
	}
	if _, err := os.Lstat(checkout.Path); !os.IsNotExist(err) {
		t.Fatal("overflow left the original checkout held", err)
	}
	repository, ok := restorer.worktrees.LinkedWorktreeRepository(checkout.Path)
	if !ok {
		t.Fatal("fixture lost managed mirror")
	}
	recoveryCLIGit(t, repository, "update-ref", "-d", record.Ref)
	if err := restorer.restore(t.Context(), writer, archive, seq); !errors.Is(err, recovery.ErrOverflowRefUnresolved) {
		t.Fatal("missing overflow pin acknowledged restoration", err)
	}
	if _, err := os.Lstat(checkout.Path); !os.IsNotExist(err) {
		t.Fatal("missing pin created an unverified checkout", err)
	}
	if err := recovery.PinCommit(t.Context(), repository, record); err != nil {
		t.Fatal(err)
	}
	if err := restorer.restore(t.Context(), writer, archive, seq); err != nil {
		t.Fatal("overflow parent restore", err)
	}
	if got := recoveryCLIGit(t, checkout.Path, "show", ":source.txt"); got != "ordinary staged" {
		t.Fatal("overflow restore lost staged state", got)
	}
	if got, err := os.ReadFile(filepath.Join(checkout.Path, "source.txt")); err != nil || string(got) != "overflow dirty\n" {
		t.Fatal("overflow restore lost working state", string(got), err)
	}
}
