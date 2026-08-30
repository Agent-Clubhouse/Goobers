package main

import (
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/journalclient"
)

// stagejournal.go is the CLI seam decision 005 R1 option 1 buys: one
// journalclient.Reader-shaped call replacing every
// `runDirFor(layoutFor(root), runID)` + `journal.OpenRead(dir)` pair in a
// stage command, and one CrossRun for the three questions that legitimately
// span runs (finding 002 C4).
//
// Selection stays in the stage, admission in the plane (DS1): the stage asks
// its environment which backend it is on, and the daemon decides what that
// backend may see. On the daemon and on type-1/type-2 hosts nothing changes —
// no endpoint is set, so the File backend opens the same run directory under
// the same reader as before. In a stage POD there is no run directory, and the
// endpoint the dispatcher stamps routes the identical calls at the daemon.
//
// The seam is fail-closed on BOTH sides of that choice: an endpoint with no
// bearer or no run identity is a refusal (journalclient.Select), and a plane
// read that fails is an error the stage surfaces — never an empty event list
// that quietly changes the stage's decision. That distinction is the whole
// reason this issue exists: terminalFailureStreak's pre-route behaviour in a
// pod was a silent 0, and a silent 0 is a policy change nobody authorised.

// stageJournalSelection resolves the stage's journal backend from the process
// environment. Split from the openers so a caller can ask "am I on the plane?"
// without opening anything.
func stageJournalSelection() (journalclient.Selection, error) {
	return journalclient.Select(os.Getenv)
}

// stageRunJournal opens the CURRENT run's journal for a stage command: the
// on-disk reader on the file path, the daemon's run-scoped read routes on the
// plane.
//
// runID names the run the caller intends to read. On the plane it must be the
// stage's own run — the token contains the read to exactly that run and the
// client refuses to build a request for another one, so a mismatch is caught
// here with a message that names both rather than as an opaque 403.
func stageRunJournal(root, runID string) (journalclient.Reader, error) {
	selection, err := stageJournalSelection()
	if err != nil {
		return nil, err
	}
	if !selection.OnPlane() {
		return journalclient.OpenFile(layoutFor(root), runID)
	}
	if runID != "" && runID != selection.RunID {
		return nil, fmt.Errorf(
			"refusing to read run %q over the journal plane: this stage is authenticated as run %q and the plane serves only its own run",
			runID, selection.RunID)
	}
	return journalclient.NewHTTPFromSelection(selection)
}

// stageCrossRunJournal returns the cross-run reader for a stage command:
// the same-host walk over the instance's run directories, or the daemon's
// three gaggle-scoped questions.
//
// warn receives non-fatal per-candidate discovery notes; it is ignored on the
// plane, where the daemon does the walking and answers with one result or one
// error.
func stageCrossRunJournal(root string, warn func(string)) (journalclient.CrossRun, error) {
	selection, err := stageJournalSelection()
	if err != nil {
		return nil, err
	}
	if !selection.OnPlane() {
		reader := journalclient.NewFileCrossRun(layoutFor(root))
		reader.Warn = warn
		return reader, nil
	}
	return journalclient.NewHTTPFromSelection(selection)
}
