package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/journalclient"
)

// stagejournal_test.go covers the CLI seam's own decisions: which backend a
// stage picks, and what it refuses. The backends themselves are tested in
// internal/journalclient and internal/httpapi; what matters here is that no
// converted reader can end up silently reading nothing.

func setPlaneEnv(t *testing.T, endpoint, token, runID, gaggle string) {
	t.Helper()
	t.Setenv(journalclient.EnvEndpoint, endpoint)
	t.Setenv(journalclient.EnvToken, token)
	t.Setenv(journalclient.EnvRunID, runID)
	t.Setenv(journalclient.EnvGaggle, gaggle)
}

// TestStageRunJournalUsesTheFilePathOffThePlane is the no-change guarantee for
// the daemon and every type-1/type-2 host: without an endpoint, the seam opens
// the same run directory the readers opened before.
func TestStageRunJournalUsesTheFilePathOffThePlane(t *testing.T) {
	setPlaneEnv(t, "", "", "", "")
	root := initDemo(t)
	runID := "seam-file-run"
	seedSeamRun(t, root, runID)

	reader, err := stageRunJournal(root, runID)
	if err != nil {
		t.Fatalf("stageRunJournal: %v", err)
	}
	if _, isFile := reader.(*journalclient.File); !isFile {
		t.Fatalf("reader = %T, want the file backend", reader)
	}
	events, err := reader.Events()
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("the seeded run read as empty")
	}
}

// TestStageRunJournalFailsClosedOnAHalfConfiguredPlane is the property that
// made this issue necessary: a pod with an endpoint but no bearer must get an
// error, never a fall-through to a run directory it does not have.
func TestStageRunJournalFailsClosedOnAHalfConfiguredPlane(t *testing.T) {
	root := initDemo(t)

	setPlaneEnv(t, "http://daemon:7777", "", "seam-run", "acme-web")
	if _, err := stageRunJournal(root, "seam-run"); !errors.Is(err, journalclient.ErrEndpointWithoutToken) {
		t.Fatalf("err = %v, want ErrEndpointWithoutToken", err)
	}
	if _, err := stageCrossRunJournal(root, nil); !errors.Is(err, journalclient.ErrEndpointWithoutToken) {
		t.Fatalf("cross-run err = %v, want ErrEndpointWithoutToken", err)
	}

	setPlaneEnv(t, "http://daemon:7777", "tok", "", "acme-web")
	if _, err := stageRunJournal(root, "seam-run"); !errors.Is(err, journalclient.ErrEndpointWithoutRun) {
		t.Fatalf("err = %v, want ErrEndpointWithoutRun", err)
	}
}

// TestStageRunJournalRefusesAnotherRunOverThePlane keeps the containment
// client-side too: the plane serves the token's run, so asking for a different
// one is a bug the seam names rather than an opaque 403 later.
func TestStageRunJournalRefusesAnotherRunOverThePlane(t *testing.T) {
	root := initDemo(t)
	setPlaneEnv(t, "http://daemon:7777", "tok", "my-run", "acme-web")

	_, err := stageRunJournal(root, "someone-elses-run")
	if err == nil {
		t.Fatal("reading another run over the plane was allowed")
	}
	if !strings.Contains(err.Error(), "someone-elses-run") || !strings.Contains(err.Error(), "my-run") {
		t.Fatalf("err = %v; the refusal must name both runs", err)
	}

	// Its own run is fine, and picks the plane backend.
	reader, err := stageRunJournal(root, "my-run")
	if err != nil {
		t.Fatalf("stageRunJournal for its own run: %v", err)
	}
	if _, isHTTP := reader.(*journalclient.HTTP); !isHTTP {
		t.Fatalf("reader = %T, want the plane backend", reader)
	}
}

// TestStageCrossRunJournalPicksTheRightBackend pins the cross-run selection.
func TestStageCrossRunJournalPicksTheRightBackend(t *testing.T) {
	root := initDemo(t)

	setPlaneEnv(t, "", "", "", "")
	reader, err := stageCrossRunJournal(root, nil)
	if err != nil {
		t.Fatalf("file cross-run: %v", err)
	}
	if _, isFile := reader.(*journalclient.FileCrossRun); !isFile {
		t.Fatalf("reader = %T, want the file cross-run backend", reader)
	}

	setPlaneEnv(t, "http://daemon:7777", "tok", "my-run", "acme-web")
	reader, err = stageCrossRunJournal(root, nil)
	if err != nil {
		t.Fatalf("plane cross-run: %v", err)
	}
	if _, isHTTP := reader.(*journalclient.HTTP); !isHTTP {
		t.Fatalf("reader = %T, want the plane cross-run backend", reader)
	}
}

// TestConvertedReadersFailClosedOnAHalfConfiguredPlane is the end-to-end
// version of the same property, through the actual converted readers: none of
// them may answer "there is nothing" when the truth is "I could not look".
func TestConvertedReadersFailClosedOnAHalfConfiguredPlane(t *testing.T) {
	root := initDemo(t)
	setPlaneEnv(t, "http://daemon:7777", "", "seam-run", "acme-web")

	if _, err := readLatestGateVerdict(root, "seam-run", "merge-review"); err == nil {
		t.Error("apply-verdict's reader degraded silently")
	}
	if _, err := readLatestRemediationBrief(root, "seam-run"); err == nil {
		t.Error("gather-issue-context's reader degraded silently")
	}
	if _, err := readRemediationBriefArtifact(root, "seam-run", "gather-ci-failures"); err == nil {
		t.Error("gather-ci-failures' reader degraded silently")
	}
	if _, _, _, err := readRemediationResponseInputs(root, "seam-run", false); err == nil {
		t.Error("respond-to-findings' reader degraded silently")
	}
	// gate-removal-guard's two readers tolerate a MISSING journal by design;
	// an unreadable one is a different thing and must be reported.
	if _, err := findingMetaFromJournal(root, "seam-run"); err == nil {
		t.Error("gate-removal-guard's finding reader degraded silently")
	}
	if _, _, err := gateEditClassificationFromJournal(root, "seam-run"); err == nil {
		t.Error("gate-removal-guard's classification reader degraded silently")
	}
}

// TestGateRemovalGuardReadersStillTolerateAMissingRun guards the other
// direction: making unreadable journals loud must not make an ABSENT one loud,
// or every non-tutor workflow starts failing.
func TestGateRemovalGuardReadersStillTolerateAMissingRun(t *testing.T) {
	setPlaneEnv(t, "", "", "", "")
	root := initDemo(t)

	if meta, err := findingMetaFromJournal(root, "no-such-run"); err != nil {
		t.Errorf("a run with no journal is not an error: %v", err)
	} else if meta.Subject != "" {
		t.Errorf("meta = %+v, want empty", meta)
	}
	kind, subject, err := gateEditClassificationFromJournal(root, "no-such-run")
	if err != nil || kind != "" || subject != "" {
		t.Errorf("classification = (%q, %q, %v), want empty and no error", kind, subject, err)
	}
}

// seedSeamRun writes a minimal run journal in the demo instance's default
// gaggle so the file backend has something to resolve.
func seedSeamRun(t *testing.T, root, runID string) {
	t.Helper()
	run, err := journal.Create(layoutFor(root).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "implementation",
	}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := run.Append(journal.Event{Type: journal.EventRunFinished, Status: "success"}); err != nil {
		t.Fatalf("append run.finished: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatalf("close run: %v", err)
	}
}
