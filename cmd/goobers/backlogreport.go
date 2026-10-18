package main

import (
	"io"
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
	if code := writeStageResultJSON(stderr, path, report, stageResultOptions{
		MarshalLabel:    "write candidate report " + path,
		WriteLabel:      "write candidate report",
		Indented:        true,
		TrailingNewline: true,
	}); code != 0 {
		return code
	}
	return 0
}
