package main

import (
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// materializePodContext has already checked the captured bytes and digest.
// Require the host's exact same-run diff pointer, never a lookalike external
// reference or another stage's evidence. The host handles genuinely empty
// implementation diffs before dispatching a reviewer.
func requireCapturedChildReviewDiff(env apiv1.InvocationEnvelope, stage string) error {
	for _, pointer := range env.ContextPointers {
		if pointer.Name == stage+".diff" && pointer.RunID == "" && pointer.External == nil && pointer.Artifact != nil && pointer.Artifact.Size > 0 && pointer.Artifact.MediaType == "text/x-diff" && pointer.Artifact.Validate() == nil {
			return nil
		}
	}
	return errors.New("generated child reviewer is missing its captured host diff")
}
