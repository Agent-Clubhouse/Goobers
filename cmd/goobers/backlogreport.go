package main

import (
	"encoding/json"
	"io"
	"os"
	"time"

	"github.com/goobers/goobers/providers"
)

// A report is evidence for human selection, never a claim or a ready verdict.
// Its lifetime is the stage artifact's existing run-retention policy.
type readOnlyBacklogReport struct {
	ObservedAt     time.Time            `json:"observedAt"`
	ReadOnly       bool                 `json:"readOnly"`
	CandidateCount int                  `json:"candidateCount"`
	Candidates     []providers.WorkItem `json:"candidates"`
	Truncated      bool                 `json:"truncated"`
}

func writeReadOnlyBacklogReport(items []providers.WorkItem, truncated bool, stderr io.Writer) int {
	path := providerInput("resultFile", "")
	if path == "" {
		return 0
	}
	if items == nil {
		items = []providers.WorkItem{}
	}
	report := readOnlyBacklogReport{
		ObservedAt: time.Now().UTC(), ReadOnly: true,
		CandidateCount: len(items), Candidates: items, Truncated: truncated,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		err = os.WriteFile(path, append(data, '\n'), 0o644)
	}
	if err != nil {
		pf(stderr, "error: write candidate report %s: %v\n", path, err)
		return 1
	}
	return 0
}
