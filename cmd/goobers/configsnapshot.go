package main

import (
	"errors"
	"fmt"
	"slices"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

func gaggleBacklogRef(set *instance.ConfigSet, gaggle string) apiv1.BacklogRef {
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == gaggle {
			return set.Gaggles[i].Spec.Backlog
		}
	}
	return apiv1.BacklogRef{}
}

func resolveGoobersForGaggle(set *instance.ConfigSet, gaggle string) (map[string]apiv1.GooberSpec, error) {
	if !slices.ContainsFunc(set.Gaggles, func(candidate apiv1.Gaggle) bool { return candidate.Name == gaggle }) {
		return nil, fmt.Errorf("gaggle %q not found in config", gaggle)
	}
	out := map[string]apiv1.GooberSpec{}
	for i := range set.Goobers {
		if set.Goobers[i].Spec.Gaggle == gaggle {
			out[set.Goobers[i].Name] = set.Goobers[i].Spec
		}
	}
	return out, nil
}

type workerConfigSnapshot struct {
	configDir     string
	generation    string
	digest        string
	cfg           *instance.Config
	set           *instance.ConfigSet
	instructions  map[string]string
	skillPackages map[string]map[string][]workflow.SkillFile
	digests       *gooberDigestIndex
}

type gooberDigestIndex struct {
	once       sync.Once
	compute    func() (map[localscheduler.WorkflowIdentity]string, error)
	byIdentity map[localscheduler.WorkflowIdentity]string
	err        error
}

func (i *gooberDigestIndex) resolve() (map[localscheduler.WorkflowIdentity]string, error) {
	i.once.Do(func() {
		if i.compute == nil {
			i.err = errors.New("no goober digest computation is wired for this config snapshot")
			return
		}
		i.byIdentity, i.err = i.compute()
	})
	return i.byIdentity, i.err
}

func (s *workerConfigSnapshot) gooberDigestFor(gaggle, workflowName string) (string, error) {
	if s.digests == nil {
		return "", errors.New("config snapshot carries no goober digest index")
	}

	byIdentity, err := s.digests.resolve()
	if err != nil {
		return "", err
	}
	digest, ok := byIdentity[localscheduler.WorkflowIdentity{Gaggle: gaggle, Workflow: workflowName}]
	if !ok {
		return "", fmt.Errorf("config tree %s declares no workflow %q in gaggle %q", s.digest, workflowName, gaggle)
	}
	return digest, nil
}

func (s *workerConfigSnapshot) instructionsFor(goobers map[string]apiv1.GooberSpec) (map[string]string, error) {
	out := make(map[string]string, len(goobers))
	for name := range goobers {
		content, ok := s.instructions[name]
		if !ok {
			return nil, fmt.Errorf("config snapshot carries no instructions for goober %q", name)
		}
		out[name] = content
	}
	return out, nil
}

func gaggleProjectRef(set *instance.ConfigSet, gaggle string) apiv1.RepoRef {
	for i := range set.Gaggles {
		if set.Gaggles[i].Name == gaggle {
			return set.Gaggles[i].Spec.Project
		}
	}
	return apiv1.RepoRef{}
}

func newGooberDigestIndex(
	cfg *instance.Config,
	set *instance.ConfigSet,
	instructions map[string]string,
	skillPackages map[string]map[string][]workflow.SkillFile,
) *gooberDigestIndex {
	return &gooberDigestIndex{compute: func() (map[localscheduler.WorkflowIdentity]string, error) {
		_, digests, _, _, err := compiledMachinesWithGooberDigests(
			set, goobersByName(set), instructions, harnessEnvironmentPolicy(cfg.Runner),
			cfg.Runner.HarnessCommand, true, nil, nil,
			func(gaggle string, _ map[string]apiv1.GooberSpec) (map[string][]workflow.SkillFile, error) {
				return skillPackages[gaggle], nil
			},
		)
		return digests, err
	}}
}

func loadSnapshotGooberInputs(configDir string, set *instance.ConfigSet) (map[string]string, map[string]map[string][]workflow.SkillFile, error) {
	goobers := goobersByName(set)
	instructions, err := loadGooberInstructions(configDir, set, goobers)
	if err != nil {
		return nil, nil, err
	}
	skillPackages := make(map[string]map[string][]workflow.SkillFile, len(set.Gaggles))
	for i := range set.Gaggles {
		gaggle := set.Gaggles[i].Name
		packages, err := loadGooberSkillPackages(configDir, gaggle, goobers)
		if err != nil {
			return nil, nil, err
		}
		skillPackages[gaggle] = packages
	}
	return instructions, skillPackages, nil
}
