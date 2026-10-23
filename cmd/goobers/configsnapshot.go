package main

import (
	"errors"
	"fmt"
	"sync"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

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

func newGooberDigestIndex(
	cfg *instance.Config,
	set *instance.ConfigSet,
	instructions map[string]string,
	skillPackages map[string]map[string][]workflow.SkillFile,
) *gooberDigestIndex {
	return &gooberDigestIndex{compute: func() (map[localscheduler.WorkflowIdentity]string, error) {
		_, digests, _, _, err := runtimewiring.CompiledMachinesWithGooberDigests(
			set, runtimewiring.GoobersByName(set), instructions, runtimewiring.HarnessEnvironmentPolicy(cfg.Runner),
			cfg.Runner.HarnessCommand, true, nil, nil,
			func(gaggle string, _ map[string]apiv1.GooberSpec) (map[string][]workflow.SkillFile, error) {
				return skillPackages[gaggle], nil
			},
		)
		return digests, err
	}}
}
