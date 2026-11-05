package main

import (
	"encoding/json"
	"fmt"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func stageGaggleBacklogStamps(gaggles []apiv1.Gaggle) (map[string]string, error) {
	stamps := make(map[string]string, len(gaggles))
	for _, gaggle := range gaggles {
		backlog := gaggle.Spec.Backlog
		encoded, err := json.Marshal(runtimewiring.StageBacklogSettings{
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
