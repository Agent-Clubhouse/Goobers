package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestMachineLocalCitationsCoversTmpPaths is #4829's core regression: the
// shipped v0.4.0-rc.1 readiness record cited 63 /tmp paths as its evidence
// trail, none of which the pre-#4829 checker's home-directory-only regex
// caught.
func TestMachineLocalCitationsCoversTmpPaths(t *testing.T) {
	t.Parallel()
	got := machineLocalCitations([]byte("evidence is in `/tmp/goobers-rc-x/proof.json`."))
	if len(got) != 1 || got[0] != "/tmp/goobers-rc-x/proof.json" {
		t.Fatalf("machineLocalCitations = %v, want the /tmp path", got)
	}
}

// A `KEY=/tmp/path` illustrates a configuration value true on every machine
// that sets it (goobernetes-restrictions.md's `GOCACHE=/tmp/gocache`) — not a
// pointer into this one author's own run, which is what the check exists to
// catch. Widening the regex to /tmp must not flag it.
func TestMachineLocalCitationsExcludesConfigValueAssignment(t *testing.T) {
	t.Parallel()
	got := machineLocalCitations([]byte("a pod whose image sets `GOCACHE=/tmp/gocache` gets a fresh cache."))
	if len(got) != 0 {
		t.Fatalf("machineLocalCitations = %v, want none for a KEY=/tmp/path assignment", got)
	}
}

// TestCheckMachineLocalCitationsFlagsUndisclaimedTmpEvidence exercises the
// docs/releases walk end to end: unlike loadDocuments/validate, it must not
// require design-document lifecycle metadata (Status, Delivered-by, ...) —
// docs/releases holds release notes and readiness records, not design docs.
func TestCheckMachineLocalCitationsFlagsUndisclaimedTmpEvidence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "v9.9.9-readiness.md")
	writeDocumentAt(t, path, "# v9.9.9 readiness\n\nEvidence is in `/tmp/goobers-rc-x/proof.json`.\n")

	problems := checkMachineLocalCitations(root)
	if len(problems) != 1 || !strings.Contains(problems[0], externalEvidenceDisclaimer) {
		t.Fatalf("problems = %v, want one external-evidence complaint", problems)
	}

	writeDocumentAt(t, path, "# v9.9.9 readiness\n\nEvidence is in `/tmp/goobers-rc-x/proof.json` "+
		"(not reproducible from this repository).\n")
	if problems := checkMachineLocalCitations(root); len(problems) != 0 {
		t.Fatalf("problems = %v, want none once disclaimed", problems)
	}
}

// A release note with no design-doc header (no `> Status:` line) must not be
// rejected for missing lifecycle metadata — only for undisclaimed citations.
func TestCheckMachineLocalCitationsIgnoresDocsWithoutCitations(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeDocumentAt(t, filepath.Join(root, "sample-release-notes.md"), "# Goobers v0.2.0\n\nNo local paths here.\n")

	if problems := checkMachineLocalCitations(root); len(problems) != 0 {
		t.Fatalf("problems = %v, want none for a doc with no citations and no status header", problems)
	}
}

func TestCheckMachineLocalCitationsReportsUnreadableRoot(t *testing.T) {
	t.Parallel()
	if problems := checkMachineLocalCitations(filepath.Join(t.TempDir(), "does-not-exist")); len(problems) == 0 {
		t.Fatal("problems = [], want a report naming the missing root")
	}
}
