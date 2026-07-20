package main

import "github.com/goobers/goobers/internal/journal"

// claimedIssueFromJournal recovers the issue this run claimed — its id and title
// — from the run journal. This is resume-safe, unlike the claim ledger (which
// close-out releases as its last step): the claiming stage's flat result file
// (claimed-item.json, a marshaled providers.WorkItem) has its scalar fields
// merged into that stage's journaled stage.finished Outputs by
// executor.mergeResultFileOutputs, so the claimed item's "id"/"title" persist in
// the journal for any later stage (open-pr, #241) to read without a fragile
// single-hop InputsFrom chain from the claiming stage.
//
// ok is false when no stage in the run produced a claimed item (a workflow that
// doesn't claim, or a run whose journal is unreadable) — callers fall back to
// their generic behavior rather than failing.
func claimedIssueFromJournal(root, runID string) (id, title string, ok bool) {
	dir, err := runDirFor(layoutFor(root), runID)
	if err != nil {
		return "", "", false
	}
	rd, err := journal.OpenRead(dir)
	if err != nil {
		return "", "", false
	}
	events, err := rd.Events()
	if err != nil {
		return "", "", false
	}
	// The first stage.finished carrying both a string "id" and "title" is the
	// claim stage (only a claimed WorkItem contributes both); take the earliest
	// so a later stage that happened to emit an "id" can't shadow it.
	for _, ev := range events {
		if ev.Type != journal.EventStageFinished || ev.Outputs == nil {
			continue
		}
		gotID, idOK := ev.Outputs["id"].(string)
		gotTitle, titleOK := ev.Outputs["title"].(string)
		if idOK && titleOK && gotID != "" {
			return gotID, gotTitle, true
		}
	}
	return "", "", false
}
