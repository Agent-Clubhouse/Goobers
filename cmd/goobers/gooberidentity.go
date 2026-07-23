package main

import (
	"fmt"
	"os"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/workflow"
)

func loadGooberInstructions(configDir string, goobers map[string]apiv1.GooberSpec) (map[string]string, error) {
	names := make([]string, 0, len(goobers))
	for name := range goobers {
		names = append(names, name)
	}
	sort.Strings(names)
	instructions := make(map[string]string, len(goobers))
	for _, name := range names {
		content, err := os.ReadFile(instructionsPath(configDir, goobers[name], name))
		if err != nil {
			return nil, fmt.Errorf("read goober %q instructions: %w", name, err)
		}
		instructions[name] = string(content)
	}
	return instructions, nil
}

func compiledMachinesWithGooberDigests(
	set *instance.ConfigSet,
	goobers map[string]apiv1.GooberSpec,
	instructions map[string]string,
) (map[localscheduler.WorkflowIdentity]*workflow.Machine, map[localscheduler.WorkflowIdentity]string, error) {
	machines, err := compiledMachines(set, goobers)
	if err != nil {
		return nil, nil, err
	}
	gooberDigests := make(map[localscheduler.WorkflowIdentity]string, len(machines))
	for identity, machine := range machines {
		digest, err := workflow.ComputeGooberDigest(machine.Def, goobers, instructions)
		if err != nil {
			return nil, nil, fmt.Errorf("digest workflow %q goobers: %w", identity.Workflow, err)
		}
		gooberDigests[identity] = digest
	}
	return machines, gooberDigests, nil
}
