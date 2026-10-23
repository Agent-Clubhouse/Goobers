package main

import (
	"encoding/json"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type stageBacklogSettings struct {
	Provider   apiv1.Provider           `json:"provider,omitempty"`
	Project    string                   `json:"project,omitempty"`
	Labels     []string                 `json:"labels,omitempty"`
	DoneStates *apiv1.BacklogDoneStates `json:"doneStates,omitempty"`
}

func stageGaggleBacklogStamps(gaggles []apiv1.Gaggle) (map[string]string, error) {
	stamps := make(map[string]string, len(gaggles))
	for _, gaggle := range gaggles {
		backlog := gaggle.Spec.Backlog
		encoded, err := json.Marshal(stageBacklogSettings{
			Provider: backlog.Provider, Project: backlog.Project,
			Labels: backlog.Labels, DoneStates: backlog.DoneStates,
		})
		if err != nil {
			return nil, fmt.Errorf("encode gaggle %q backlog settings: %w", gaggle.Name, err)
		}
		stamps[gaggle.Name] = string(encoded)
	}
	return stamps, nil
}
