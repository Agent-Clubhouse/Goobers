package workbenchservice

import (
	"context"
	"strings"

	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchprovider"
)

// Inspect returns an observation and mechanically known wait reasons. It never
// asserts that an answer is semantically sufficient; the agent assesses that.
func (s *SessionResolver) Inspect(ctx context.Context, binding string, request workbench.BacklogItemRequest) (workbench.NeedsHumanObservation, error) {
	var result workbench.NeedsHumanObservation
	err := s.use(ctx, binding, func(ctx context.Context, bound ReadBinding) error {
		return s.service.WithLearnedBlock(ctx, resolutionRepository(bound), request.ID, func(ctx context.Context, learned LearnedBlock) error {
			adapter, err := s.adapter(ctx, bound)
			if err != nil {
				return err
			}
			result, err = s.inspect(ctx, adapter, request, learned)
			return err
		})
	})
	return result, resolutionError(err)
}
func (s *SessionResolver) inspect(ctx context.Context, adapter *workbenchprovider.AttentionResolver, request workbench.BacklogItemRequest, learned LearnedBlock) (workbench.NeedsHumanObservation, error) {
	result, err := adapter.Inspect(ctx, request, learned.Blockers)
	if err != nil {
		return result, err
	}
	result.LearnedRecordDigest = learned.Digest
	result.LearnedReason = learned.Reason
	result.LearnedComplete = result.LearnedComplete && learned.Complete
	result.HumanInstruction = &workbench.NeedsHumanEvidenceRef{Kind: "current-human-message", ID: s.origin.MessageID, Digest: s.origin.MessageDigest}
	result.WaitReasons = resolutionWaitReasons(result)
	result.Digest, err = workbench.NeedsHumanObservationDigest(result)
	return result, err
}
func resolutionWaitReasons(o workbench.NeedsHumanObservation) []string {
	reasons := []string{}
	if !o.MarkerPresent {
		reasons = append(reasons, "needs-human-marker-absent")
	}
	if !o.CommentsComplete {
		reasons = append(reasons, "source-comments-incomplete")
	}
	if !o.DependenciesComplete {
		reasons = append(reasons, "native-dependencies-incomplete")
	}
	if !o.LearnedComplete {
		reasons = append(reasons, "learned-block-record-incomplete")
	}
	if dependenciesUnresolved(o.Dependencies) || dependenciesUnresolved(o.LearnedDependencies) {
		reasons = append(reasons, "known-dependency-unresolved")
	}
	// A legacy marker may use the actual current human instruction as its basis.
	if strings.TrimSpace(o.LearnedReason) == "" && o.HumanInstruction == nil {
		reasons = append(reasons, "resolution-reason-missing")
	}
	return reasons
}
func dependenciesUnresolved(items []workbench.NeedsHumanDependency) bool {
	for _, item := range items {
		if !item.Verified || item.Open {
			return true
		}
	}
	return false
}
