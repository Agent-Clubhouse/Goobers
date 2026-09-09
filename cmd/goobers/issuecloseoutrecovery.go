package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journalclient"
	"github.com/goobers/goobers/internal/recovery"
)

func issueCloseOutEvidenceDetail(root, runID string) (string, error) {
	verdict, gateName, found, err := issueCloseOutReviewVerdict(root, runID)
	if err != nil {
		return "", fmt.Errorf("read review verdict: %w", err)
	}
	var detail string
	if found {
		detail = issueCloseOutVerdictDetail(verdict, gateName, runID)
	}
	retained, err := issueCloseOutRecoveryDetail(root, runID)
	if err != nil {
		return "", fmt.Errorf("read retained recovery: %w", err)
	}
	return detail + retained, nil
}

func issueCloseOutRecoveryDetail(root, runID string) (string, error) {
	reader, err := issueCloseOutJournal(root, runID)
	if errors.Is(err, journalclient.ErrRunNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	events, err := reader.Events()
	if err != nil {
		return "", err
	}
	records, err := recovery.RecordsFromEvents(events, runID)
	if err != nil || len(records) == 0 {
		return "", err
	}
	var out strings.Builder
	out.WriteString("\n\nRetained recovery state (restore verifies archive availability and integrity):\n")
	for _, record := range records {
		fmt.Fprintf(&out, "\n- Ref: `%s`\n  Patch: `%s`\n  Base: `%s`\n  Retain until: `%s`\n", record.Ref, record.PatchDigest, record.BaseSHA, record.RetainUntil.UTC().Format(time.RFC3339Nano))
		if out.Len() > 32<<10 {
			return "", fmt.Errorf("recovery close-out detail exceeds comment budget")
		}
	}
	return out.String(), nil
}
