package runtimewiring

import (
	"errors"
	"fmt"
	"sync"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

// ConfigSnapshot captures immutable config inputs shared by admission and execution.
type ConfigSnapshot struct {
	ConfigDirectory string
	Generation      string
	Digest          string
	Config          *instance.Config
	Definitions     *instance.ConfigSet
	Instructions    map[string]string
	SkillPackages   map[string]map[string][]workflow.SkillFile
	Digests         *GooberDigestIndex
}

// GooberDigestIndex lazily computes workflow identities once per captured tree.
type GooberDigestIndex struct {
	once       sync.Once
	compute    func() (map[localscheduler.WorkflowIdentity]string, error)
	byIdentity map[localscheduler.WorkflowIdentity]string
	err        error
}

func (i *GooberDigestIndex) resolve() (map[localscheduler.WorkflowIdentity]string, error) {
	i.once.Do(func() {
		if i.compute == nil {
			i.err = errors.New("no goober digest computation is wired for this config snapshot")
			return
		}
		i.byIdentity, i.err = i.compute()
	})
	return i.byIdentity, i.err
}

// GooberDigestFor resolves a workflow pin from the captured tree.
func (s *ConfigSnapshot) GooberDigestFor(gaggle, workflowName string) (string, error) {
	if s.Digests == nil {
		return "", errors.New("config snapshot carries no goober digest index")
	}

	byIdentity, err := s.Digests.resolve()
	if err != nil {
		return "", err
	}
	digest, ok := byIdentity[localscheduler.WorkflowIdentity{Gaggle: gaggle, Workflow: workflowName}]
	if !ok {
		return "", fmt.Errorf("config tree %s declares no workflow %q in gaggle %q", s.Digest, workflowName, gaggle)
	}
	return digest, nil
}

// NewGooberDigestIndex uses the same deferred discovery policy for daemon and worker pins.
func NewGooberDigestIndex(
	cfg *instance.Config,
	set *instance.ConfigSet,
	instructions map[string]string,
	skillPackages map[string]map[string][]workflow.SkillFile,
) *GooberDigestIndex {
	return &GooberDigestIndex{
		compute: func() (map[localscheduler.WorkflowIdentity]string, error) {
			_, digests, _, _, err := CompiledMachinesWithGooberDigests(
				set,
				GoobersByName(set),
				instructions,
				HarnessEnvironmentPolicy(cfg.Runner),
				cfg.Runner.HarnessCommand,
				// The daemon defers model discovery when it computes the
				// digests this pin is matched against. Deferring here too is
				// not an optimization: a worker that probed the launcher for a
				// model the daemon took from config would resolve a different
				// GooberSpec.Model, digest it, and refuse every attempt of
				// every run as a mismatch.
				true,
				// nil, not built: DeferDiscovery (above) short-circuits before
				// any credential is ever consulted, so resolving one here would
				// be pure overhead on this hot per-reload path (#4292).
				nil,
				// nil: the connector check (#4475) is an authoring-time
				// rejection the daemon already applied; it never changes a
				// digest, so re-running it here could only refuse attempts.
				nil,
				func(gaggle string, _ map[string]apiv1.GooberSpec) (map[string][]workflow.SkillFile, error) {
					return skillPackages[gaggle], nil
				},
			)
			if err != nil {
				return nil, err
			}
			return digests, nil
		},
	}
}

// InstructionsFor selects captured instructions and refuses missing profiles.
func (s *ConfigSnapshot) InstructionsFor(goobers map[string]apiv1.GooberSpec) (map[string]string, error) {
	out := make(map[string]string, len(goobers))
	for name := range goobers {
		content, ok := s.Instructions[name]
		if !ok {
			return nil, &GooberInstructionsError{
				Goober: name,
				Err:    fmt.Errorf("config tree %s captured no instructions for this goober", s.Digest),
			}
		}
		out[name] = content
	}
	return out, nil
}
