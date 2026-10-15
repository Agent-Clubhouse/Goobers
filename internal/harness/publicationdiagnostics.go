package harness

import (
	"context"
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

// Completion-boundary check outcomes beyond mcpio's accepted/rejected.
const (
	// publicationCheckErrored: the check could not judge the publication
	// (I/O, cancellation); it is neither accepted nor repairable.
	publicationCheckErrored = "errored"
	// Stable codes for an errored check.
	publicationCheckCancelled = "completion_check_cancelled"
	publicationCheckFailed    = "completion_check_failed"
)

// publicationCheck is one completion-boundary validation of the declared
// publication. Like publish_output receipts it names slots, schemas, codes and
// JSON Pointer locations, never payload values. PayloadDigests maps each
// schema-bound slot of an accepted check to the digest of the exact bytes
// validated at completion, which may differ from a tool-time receipt if the
// payload changed after publish_output.
type publicationCheck struct {
	Outcome        string               `json:"outcome"`
	Code           string               `json:"code,omitempty"`
	Slot           string               `json:"slot,omitempty"`
	SchemaID       string               `json:"schemaId,omitempty"`
	Issues         []handoffcheck.Issue `json:"issues,omitempty"`
	PayloadDigests map[string]string    `json:"payloadDigests,omitempty"`
}

func (p *publicationPostcondition) record(prepared *artifactset.Prepared, err error) {
	if err == nil {
		check := publicationCheck{Outcome: mcpio.PublicationAccepted}
		if prepared != nil {
			for _, slot := range p.slots {
				if digest, ok := prepared.Digest(slot); ok {
					if check.PayloadDigests == nil {
						check.PayloadDigests = map[string]string{}
					}
					check.PayloadDigests[slot] = digest
				}
			}
		}
		p.checks = append(p.checks, check)
		return
	}
	code, _, declared := declaredArtifactFailure(err)
	if !declared {
		check := publicationCheck{Outcome: publicationCheckErrored, Code: publicationCheckFailed}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			check.Code = publicationCheckCancelled
		}
		p.checks = append(p.checks, check)
		return
	}
	check := publicationCheck{Outcome: mcpio.PublicationRejected, Code: code}
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
		check.SchemaID = p.schemaIDs[publication.Slot]
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
