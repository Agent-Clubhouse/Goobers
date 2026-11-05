package workerscenarios

import (
	"encoding/json"
	"fmt"
	"io"
)

type chaosSummary struct {
	Results []chaosResult `json:"results"`
}

type chaosResult struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

// RequirePassedEvidence proves each required scenario ran and passed.
func RequirePassedEvidence(reader io.Reader, required ...string) error {
	var summary chaosSummary
	if err := json.NewDecoder(reader).Decode(&summary); err != nil {
		return fmt.Errorf("decode chaos evidence: %w", err)
	}
	results := make(map[string]string, len(summary.Results))
	for _, result := range summary.Results {
		if _, duplicate := results[result.ID]; duplicate {
			return fmt.Errorf("chaos evidence contains duplicate %s", result.ID)
		}
		results[result.ID] = result.Status
	}
	for _, id := range required {
		status, found := results[id]
		switch {
		case !found:
			return fmt.Errorf("chaos evidence does not contain %s", id)
		case status != "passed":
			return fmt.Errorf("chaos evidence reports %s as %q", id, status)
		}
	}
	return nil
}
