package runner

import (
	"errors"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workspacebranch"
	"github.com/goobers/goobers/internal/workspacerevision"
)

// validateTaskAdmission runs before any workspace or credential provisioning.
func validateTaskAdmission(tf taskFrame, branch int) error {
	t, in := tf.t, tf.in
	if branch != 0 && workspacebranch.BackendKind(t.Inputs) {
		return codedStageFailure(workspacerevision.CodeConflict, fmt.Errorf("remote branch operations must execute outside parallel branches"))
	}
	// contextFrom pointers and inputsFrom scalars have separate provenance;
	// checking only pointers would admit unapproved provider-authored scalars.
	integrityErr := apiv1.ValidateInputIntegrity(in.Item, tf.upstream, t.MinimumIntegrity)
	if integrityErr == nil {
		integrityErr = apiv1.ValidateResolvedInputIntegrity(
			resolvedInputGrades(t, in.Machine, tf.upstreamResult, tf.completed, tf.fanIn), t.MinimumIntegrity)
	}
	if err := integrityErr; err != nil {
		admission := &apiv1.IntegrityAdmissionError{}
		if !errors.As(err, &admission) {
			return err
		}
		if appendErr := tf.jr.Append(journal.Event{
			Type:             journal.EventError,
			Stage:            t.Name,
			Integrity:        admission.Actual,
			MinimumIntegrity: admission.Minimum,
			Error: &journal.ErrorDetail{
				Code: apiv1.IntegrityAdmissionErrorCode, Message: admission.Error(),
			},
		}); appendErr != nil {
			return fmt.Errorf("runner: journal integrity refusal for %q: %w", t.Name, appendErr)
		}
		return fmt.Errorf("runner: refuse stage %q: %w", t.Name, admission)
	}
	return nil
}
