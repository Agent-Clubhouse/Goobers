package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

// TestSweepOrphanedEphemeralTmpGatedOnDeclaration proves #3969's fix is
// declaration-gated exactly like the rest of the tmp:ephemeral binding
// (internal/ephemeraltmp): a self runner that does NOT declare the
// restriction sees no sweep at all, so an operator who never opted in gets a
// byte-identical startup.
func TestSweepOrphanedEphemeralTmpGatedOnDeclaration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	if runtime.GOOS == "windows" {
		t.Setenv("TMP", root)
		t.Setenv("TEMP", root)
	}
	orphan := filepath.Join(os.TempDir(), "goobers-ephemeral-tmp-leftover")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &instance.Config{Runners: []instance.RunnerEntry{
		{Name: "self", Host: instance.RunnerHostSelfName},
	}}
	sweepOrphanedEphemeralTmp(cfg, nil)

	if _, err := os.Stat(orphan); err != nil {
		t.Fatalf("orphan removed despite no tmp:ephemeral declaration: %v", err)
	}
}

// TestSweepOrphanedEphemeralTmpRemovesOrphansWhenDeclared proves the sweep
// actually runs, and reclaims a leftover directory, once the self runner
// declares tmp:ephemeral.
func TestSweepOrphanedEphemeralTmpRemovesOrphansWhenDeclared(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	if runtime.GOOS == "windows" {
		t.Setenv("TMP", root)
		t.Setenv("TEMP", root)
	}
	orphan := filepath.Join(os.TempDir(), "goobers-ephemeral-tmp-leftover")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(os.TempDir(), "not-an-orphan")
	if err := os.MkdirAll(keep, 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &instance.Config{Runners: []instance.RunnerEntry{
		{Name: "self", Host: instance.RunnerHostSelfName, Restrictions: []instance.RunnerRestriction{instance.RunnerRestrictionTmpEphemeral}},
	}}
	sweepOrphanedEphemeralTmp(cfg, nil)

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan still exists after startup sweep: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("sweep removed something it must not have: %v", err)
	}
}
