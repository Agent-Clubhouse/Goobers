package harness

import (
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
)

// PublicationDiagnosticsKind is the runner annotation that records a
// schema-bound stage's publish_output outcomes and completion-boundary checks
// for one attempt (#6868).
const PublicationDiagnosticsKind = "goobers-io-publication-diagnostics"

// maxJournaledPublicationReceipts bounds the annotation; the most recent
// receipts are kept because they explain the attempt's final state.
const maxJournaledPublicationReceipts = 32

// publicationCheck is one completion-boundary validation of the declared
// publication. Like publish_output receipts it names slots, schemas, codes and
// JSON Pointer locations, never payload values.
type publicationCheck struct {
	Outcome  string               `json:"outcome"`
	Code     string               `json:"code,omitempty"`
	Slot     string               `json:"slot,omitempty"`
	SchemaID string               `json:"schemaId,omitempty"`
	Issues   []handoffcheck.Issue `json:"issues,omitempty"`
}

func (p *publicationPostcondition) record(err error) {
	if err == nil {
		p.checks = append(p.checks, publicationCheck{Outcome: mcpio.PublicationAccepted})
		return
	}
	check := publicationCheck{Outcome: mcpio.PublicationRejected}
	check.Code, _, _ = declaredArtifactFailure(err)
	var handoff *artifactset.HandoffError
	var publication *artifactset.PublicationError
	switch {
	case errors.As(err, &handoff):
		check.Slot, check.SchemaID = handoff.Entry, handoff.SchemaID
		for _, issue := range handoff.Issues {
			check.Issues = append(check.Issues, handoffcheck.Issue{Code: issue.Code, Path: issue.Path, Keyword: issue.Keyword, Offset: issue.Offset})
		}
	case errors.As(err, &publication):
		check.Slot = publication.Slot
	}
	p.checks = append(p.checks, check)
}

// journalPublicationDiagnostics records this attempt's publish_output receipts
// and completion-boundary checks. It is a no-op for a stage without
// schema-bound slots. liftArtifacts remains the authoritative judge; this only
// explains how the attempt got there.
func (e *Executor) journalPublicationDiagnostics(env apiv1.InvocationEnvelope, req RunRequest, p *publicationPostcondition) error {
	if len(req.PublicationSchemas) == 0 {
		return nil
	}
	receipts, readErr := mcpio.ReadPublicationReceipts(req.Workspace, goobersIOPublicationReceiptFile())
	appender, ok := e.recorder.(EventAppender)
	if !ok {
		return fmt.Errorf("harness: publication diagnostics require a journal-backed recorder; %T cannot append events", e.recorder)
	}
	runner := map[string]any{"kind": PublicationDiagnosticsKind, "attempt": env.Attempt}
	if readErr != nil {
		// Without the log the call count is unknown, not zero.
		runner["receiptError"] = "publication receipts could not be read"
	} else {
		runner["publishReceipts"] = len(receipts)
		runner["receipts"] = receipts[max(0, len(receipts)-maxJournaledPublicationReceipts):]
	}
	if p != nil {
		runner["completionChecks"] = p.checks
	}
	if err := appender.Append(journal.Event{Type: journal.EventRunnerAnnotation, Stage: env.TaskID, Runner: runner}); err != nil {
		return fmt.Errorf("harness: journal publication diagnostics for %q: %w", env.TaskID, err)
	}
	return nil
}
