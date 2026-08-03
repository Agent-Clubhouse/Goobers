// Package decomposition holds shared types and logic for the decomposition
// workflow's deterministic stages (docs/design/decomposition-workflow.md):
// select-source (DEC-1) and validate-plan (DEC-2). Publication (DEC-3/4) and
// the workflow definition itself (DEC-5/6) are later slices and are not
// implemented here.
package decomposition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

// SelectionMode identifies how select-source found its target parent. Only
// SelectionModeEscalation is produced today; SelectionModePointed is DEC-6.
type SelectionMode string

// SelectionModeEscalation and SelectionModePointed are the two entry modes
// design doc §2/§3.1 describes.
const (
	SelectionModeEscalation SelectionMode = "escalation"
	SelectionModePointed    SelectionMode = "pointed"
)

// RecognizedErrorCodes are the non-retryable L6 escalation dispositions
// (#415) select-source consumes. Deliberately mirrors, rather than imports,
// internal/runner's escalateErrorCodes: the runner owns routing policy for a
// live run, this package owns read-side recognition of its durable trace: two
// independent copies of a two-entry set are cheaper to keep in sync by
// inspection than a cross-package dependency from the read side back into the
// runner would be worth.
var RecognizedErrorCodes = map[string]bool{
	"ISSUE_OVER_SCOPE":    true,
	"NEEDS_DECOMPOSITION": true,
}

// claimStageName is the stage name convention every shipped and example
// workflow uses for its `goobers backlog-query --claim` stage (the CLI
// command is backlog-query; the stage — and therefore the journaled Stage
// field — is named query-backlog: see implementation.yaml/backlog-curation.yaml
// in selfhost/ and config-examples/). Its declared resultFile (a
// providers.WorkItem) is merged into that stage's journaled Outputs by
// internal/executor's mergeResultFileOutputs, so Outputs["id"] is the claimed
// item's provider-native ID.
const claimStageName = "query-backlog"

// ParentRef identifies the decomposition's target issue as observed at
// selection time.
type ParentRef struct {
	Provider         string `json:"provider"`
	Repository       string `json:"repository"`
	ID               string `json:"id"`
	ObservedRevision string `json:"observedRevision"`
}

// Selection is the immutable artifact select-source emits (design doc §2).
// The error message is evidence for the decomposer, not executable
// instruction.
type Selection struct {
	Mode                SelectionMode `json:"mode"`
	SourceRunID         string        `json:"sourceRunId,omitempty"`
	SourceWorkflow      string        `json:"sourceWorkflow,omitempty"`
	SourceStage         string        `json:"sourceStage,omitempty"`
	ErrorCode           string        `json:"errorCode,omitempty"`
	ErrorMessage        string        `json:"errorMessage,omitempty"`
	Parent              ParentRef     `json:"parent"`
	IssueSnapshotDigest string        `json:"issueSnapshotDigest"`
}

// EscalationCandidate is one unconsumed L6 escalation disposition eligible
// for decomposition, before the live parent has been fetched or claimed.
type EscalationCandidate struct {
	SourceRunID    string
	SourceWorkflow string
	SourceStage    string
	ErrorCode      string
	ErrorMessage   string
	StartedAt      time.Time
	ParentProvider string
	ParentID       string
}

// FindEscalationCandidates scans every escalated run's current, unrecovered
// terminal segment (readservice.OfflineRuns already derives that segment the
// same way the run-operator surfaces do — see EscalationCause/currentLifecycleRecords)
// for a non-retryable L6 disposition, and returns the eligible ones oldest
// first (design doc §2.1: "the oldest eligible run owns the first pass").
//
// A run only qualifies when its escalation cause is a stage failure (Selector
// Kind "stage"), not a gate-mediated one (repass-budget exhaustion, reviewer
// rejection) or a bare condition error — and that stage's own recorded error
// code is one of RecognizedErrorCodes with Status "failure" specifically, not
// "blocked" (a dependency block). Because the runner (#415) routes a
// non-retryable recognized-code failure straight to PhaseEscalated — bypassing
// the Next gate and its repass loop entirely — reaching PhaseEscalated through
// exactly this event shape is definitionally proof of retryable:false; the
// journal does not separately persist the Retryable bit (internal/journal.ErrorDetail
// carries only Code/Message), so this shape check is the read side's only
// available proxy for it, not an approximation of a strictly weaker check.
//
// A run that later resumed and completed does not appear here at all: ListRuns
// filters on the run's CURRENT phase, which a completed resume reports as
// PhaseCompleted, not PhaseEscalated.
func FindEscalationCandidates(ctx context.Context, reads readservice.OfflineRuns) ([]EscalationCandidate, error) {
	runs, err := listEscalatedRuns(ctx, reads)
	if err != nil {
		return nil, err
	}

	candidates := make([]EscalationCandidate, 0, len(runs))
	for _, run := range runs {
		detail, err := reads.GetRun(ctx, run.ID)
		if err != nil {
			return nil, fmt.Errorf("decomposition: get run %q: %w", run.ID, err)
		}
		if detail.Escalation == nil || detail.Escalation.Selector.Kind != "stage" {
			continue
		}
		events, err := reads.RunEvents(ctx, run.ID)
		if err != nil {
			return nil, fmt.Errorf("decomposition: get run events %q: %w", run.ID, err)
		}
		causal := findEventBySeq(events.Events, detail.Escalation.CausalEventSeq)
		if causal == nil ||
			causal.Type != journal.EventStageFinished ||
			causal.Status != string(apiv1.ResultFailure) ||
			causal.Error == nil ||
			!RecognizedErrorCodes[causal.Error.Code] {
			continue
		}
		parentProvider, parentID, ok := findClaimedParent(events.Events)
		if !ok {
			continue
		}
		candidates = append(candidates, EscalationCandidate{
			SourceRunID:    run.ID,
			SourceWorkflow: run.Workflow,
			SourceStage:    causal.Stage,
			ErrorCode:      causal.Error.Code,
			ErrorMessage:   causal.Error.Message,
			StartedAt:      run.StartedAt,
			ParentProvider: parentProvider,
			ParentID:       parentID,
		})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].StartedAt.Before(candidates[j].StartedAt)
	})
	return candidates, nil
}

func listEscalatedRuns(ctx context.Context, reads readservice.OfflineRuns) ([]readservice.RunSummary, error) {
	var runs []readservice.RunSummary
	cursor := ""
	for {
		page, err := reads.ListRuns(ctx, readservice.RunListOptions{
			Phase:  journal.PhaseEscalated,
			Limit:  200,
			Cursor: cursor,
		})
		if err != nil {
			return nil, fmt.Errorf("decomposition: list escalated runs: %w", err)
		}
		runs = append(runs, page.Runs...)
		if page.NextCursor == "" {
			return runs, nil
		}
		cursor = page.NextCursor
	}
}

func findEventBySeq(events []readservice.RunEvent, seq uint64) *readservice.RunEvent {
	for i := range events {
		if events[i].Seq == seq {
			return &events[i]
		}
	}
	return nil
}

// findClaimedParent locates the claim stage's successful result within a
// run's event stream and reports the provider-native issue it claimed.
// Outputs["id"] is a string because providers.WorkItem.ID is a string (a
// bare issue/work-item number, not always numeric across providers);
// Outputs["provider"] is the provider kind backlog-query's own claimed-item
// result file records.
func findClaimedParent(events []readservice.RunEvent) (provider, id string, ok bool) {
	for i := range events {
		event := events[i]
		if !event.KnownSchema ||
			event.Type != journal.EventStageFinished ||
			event.Stage != claimStageName ||
			event.Status != string(apiv1.ResultSuccess) {
			continue
		}
		idValue, ok := event.Outputs["id"].(string)
		if !ok || idValue == "" {
			continue
		}
		providerValue, _ := event.Outputs["provider"].(string)
		return providerValue, idValue, true
	}
	return "", "", false
}

// IssueSnapshotDigest computes the immutable selection artifact's digest over
// the parent issue content that made it eligible (title, body, labels,
// state) — the fields a conflicting concurrent edit (design doc §4's
// "live-parent-changed" check) must be compared against. It deliberately
// excludes volatile fields such as UpdatedAt: the digest identifies the
// content the decomposition plan was designed against, not exactly when it
// was last touched.
func IssueSnapshotDigest(id, title, body string, labels []string, state string) (string, error) {
	sorted := append([]string(nil), labels...)
	sort.Strings(sorted)
	data, err := json.Marshal(struct {
		ID     string   `json:"id"`
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
		State  string   `json:"state"`
	}{ID: id, Title: title, Body: body, Labels: sorted, State: state})
	if err != nil {
		return "", fmt.Errorf("decomposition: marshal issue snapshot: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}
