package main

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/worktree"
)

func TestRecoveryCleanupUsesDurableBaseRef(t *testing.T) {
	for _, baseRef := range []string{
		"refs/heads/main",
		"refs/heads/master",
		"refs/heads/release/2026.09",
		"refs/remotes/mirror/main",
		"refs/remotes/mirror/master",
		"refs/remotes/mirror/release/2026.09",
		"refs/tags/v1.0.0",
		"0123456789012345678901234567890123456789",
	} {
		t.Run(strings.ReplaceAll(baseRef, "/", "_"), func(t *testing.T) {
			got, err := recoveryCleanupBaseRef(worktree.CleanupTarget{BaseRef: baseRef})
			if err != nil || got != baseRef {
				t.Fatalf("recoveryCleanupBaseRef() = %q, %v; want %q", got, err, baseRef)
			}
		})
	}
}

func TestRecoveryCleanupRefusesMissingDurableBaseRef(t *testing.T) {
	if _, err := recoveryCleanupBaseRef(worktree.CleanupTarget{}); err == nil {
		t.Fatal("missing owning-run base reference permitted recovery cleanup")
	}
}
