package runner

import (
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func recoveryVerdictResolver(jr executionJournal) func(string) (*apiv1.Verdict, error) {
	return func(gateName string) (*apiv1.Verdict, error) {
		reader, err := journal.OpenRead(jr.Dir())
		if err != nil {
			return nil, err
		}
		events, err := reader.Events()
		if err != nil {
			return nil, err
		}
		branch := 0
		if scoped, ok := jr.(*branchJournal); ok {
			branch = scoped.branch
		}
		return latestRecoveryVerdict(events, gateName, branch, reader.ArtifactBytes)
	}
}

func latestRecoveryVerdict(events []journal.Event, gateName string, branch int, artifact func(journal.Ref) ([]byte, error)) (*apiv1.Verdict, error) {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		// Branch numbers are reused by later parallel blocks. Never borrow
		// evidence from a previous block or from a sibling branch.
		if branch != 0 && event.Type == journal.EventParallelStarted {
			break
		}
		if event.Branch != branch || event.Type != journal.EventGateEvaluated || event.Gate != gateName {
			continue
		}
		if event.Ref == nil {
			return nil, nil
		}
		data, err := artifact(*event.Ref)
		if err != nil {
			return nil, err
		}
		var verdict apiv1.Verdict
		if err := json.Unmarshal(data, &verdict); err != nil {
			return nil, fmt.Errorf("decode recovery verdict: %w", err)
		}
		return &verdict, nil
	}
	return nil, nil
}
