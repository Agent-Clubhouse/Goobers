package bandit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/journal"
)

// LoadFileObservations reads the local runner's durable observation events.
func LoadFileObservations(runsDir string, stage string) ([]Observation, error) {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return nil, fmt.Errorf("read experiment journals: %w", err)
	}
	var observations []Observation
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		reader, err := journal.OpenReadOnly(filepath.Join(runsDir, entry.Name()))
		if err != nil {
			if errors.Is(err, journal.ErrNotRunDirectory) {
				continue
			}
			return nil, fmt.Errorf("open experiment journal %q: %w", entry.Name(), err)
		}
		events, err := reader.Events()
		if err != nil {
			return nil, fmt.Errorf("read experiment journal %q: %w", entry.Name(), err)
		}
		for _, event := range events {
			if event.Type != journal.EventBanditObservation || event.Stage != stage {
				continue
			}
			data, err := json.Marshal(event.Outputs)
			if err != nil {
				return nil, fmt.Errorf("encode experiment observation: %w", err)
			}
			var observation Observation
			if err := json.Unmarshal(data, &observation); err != nil {
				return nil, fmt.Errorf("decode experiment observation: %w", err)
			}
			observations = append(observations, observation)
		}
	}
	SortObservations(observations)
	return observations, nil
}
