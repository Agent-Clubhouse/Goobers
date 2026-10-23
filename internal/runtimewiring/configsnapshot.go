package runtimewiring

import (
	"fmt"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workflow"
)

// GaggleBacklogRef returns the configured backlog for a gaggle.
func GaggleBacklogRef(set *instance.ConfigSet, gaggle string) apiv1.BacklogRef {
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == gaggle {
			return set.Gaggles[i].Spec.Backlog
		}
	}
	return apiv1.BacklogRef{}
}

// ResolveGoobersForGaggle selects gaggle-local and shared goober definitions.
func ResolveGoobersForGaggle(set *instance.ConfigSet, gaggle string) (map[string]apiv1.GooberSpec, error) {
	if !slices.ContainsFunc(set.Gaggles, func(candidate apiv1.Gaggle) bool { return candidate.Name == gaggle }) {
		return nil, fmt.Errorf("gaggle %q not found in config", gaggle)
	}
	out := map[string]apiv1.GooberSpec{}
	for i := range set.Goobers {
		if set.Goobers[i].Spec.Gaggle == "" || set.Goobers[i].Spec.Gaggle == gaggle {
			out[set.Goobers[i].Name] = set.Goobers[i].Spec
		}
	}
	return out, nil
}

// GaggleProjectRef returns the repository reference for a gaggle.
func GaggleProjectRef(set *instance.ConfigSet, gaggle string) apiv1.RepoRef {
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == gaggle {
			return set.Gaggles[i].Spec.Project
		}
	}
	return apiv1.RepoRef{}
}

// LoadSnapshotGooberInputs captures persona instructions and skill packages from a config tree.
func LoadSnapshotGooberInputs(configDir string, set *instance.ConfigSet) (map[string]string, map[string]map[string][]workflow.SkillFile, error) {
	goobers := GoobersByName(set)
	instructions, err := LoadGooberInstructions(configDir, set, goobers)
	if err != nil {
		return nil, nil, err
	}
	skillPackages := make(map[string]map[string][]workflow.SkillFile, len(set.Gaggles))
	for i := range set.Gaggles {
		gaggle := set.Gaggles[i].Name
		packages, err := LoadGooberSkillPackages(configDir, gaggle, goobers)
		if err != nil {
			return nil, nil, err
		}
		skillPackages[gaggle] = packages
	}
	return instructions, skillPackages, nil
}
