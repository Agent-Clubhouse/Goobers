package executor

import (
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
)

// InputVerificationReport opts a deterministic checker adapter into publishing
// a bounded JSON report on stdout. It does not enable proof generation.
const InputVerificationReport = "verificationReport"

func (e *ShellExecutor) verificationEnabled(env apiv1.InvocationEnvelope) (bool, error) {
	value, present := env.Inputs[InputVerificationReport]
	if !present {
		return false, nil
	}
	var enabled bool
	switch typed := value.(type) {
	case bool:
		enabled = typed
	case string:
		switch typed {
		case "true":
			enabled = true
		case "false":
		default:
			return false, errors.New("executor: verificationReport must be true or false")
		}
	default:
		return false, errors.New("executor: verificationReport must be a boolean")
	}
	if enabled {
		if _, ok := e.Journal.(credentialRefusalEventAppender); !ok {
			return false, errors.New("executor: verification reports require a journal appender")
		}
	}
	return enabled, nil
}

func (e *ShellExecutor) publishVerificationReport(env apiv1.InvocationEnvelope, data []byte, truncated bool, source journal.Ref, outcome *apiv1.ResultEnvelope, retErr *error) {
	// Nonzero exits, timeouts, cancellation, and other executor errors cannot
	// become proof evidence even when partial stdout contains a valid report.
	if *retErr != nil || outcome.Status != apiv1.ResultSuccess {
		return
	}
	report, err := creditgraph.DecodeVerificationReport(data)
	if truncated {
		err = errors.New("checker report stdout was truncated")
	}
	if err != nil {
		outcome.Status = apiv1.ResultFailure
		outcome.Error = journal.ErrorInfoFor("verification_report_invalid", err, false)
		outcome.Summary = "checker did not publish a valid verification report"
		return
	}
	appender, ok := e.Journal.(credentialRefusalEventAppender)
	if !ok {
		*retErr = errors.New("executor: verification reports require a journal appender")
		return
	}
	if err := appender.Append(journal.Event{
		Type: journal.EventRunnerAnnotation, Stage: env.TaskID, Attempt: int(env.Attempt),
		Runner: creditgraph.VerificationAnnotationPayload(report, source.Digest),
	}); err != nil {
		*retErr = fmt.Errorf("executor: record verification report: %w", err)
	}
}
