// Package startpin computes the immutable digests a cloud start is admitted
// against: the compiled workflow digest and the effective goober (persona)
// digest, from the same compile path and the same inputs the worker uses when
// it checks a pinned attempt, so a digest minted at admission is the digest a
// worker later accepts.
package startpin

import (
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	cloudconfig "github.com/goobers/goobers/internal/cloud/config"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runtimewiring"
	"github.com/goobers/goobers/internal/workflow"
)

// Compute returns the pins of every workflow in set, keyed "gaggle/workflow",
// reading goober instructions and skill packages from configDir (the generation's
// read-only tree). A workflow that does not compile fails the whole generation:
// admitting against a partial pin set would let an unpinned workflow start.
func Compute(configDir string, cfg *instance.Config, set *instance.ConfigSet) (map[string]cloudconfig.Pins, error) {
	if cfg == nil || set == nil {
		return nil, fmt.Errorf("startpin: a loaded configuration is required")
	}
	// The worker reload, the daemon and child admission all compile the set
	// AFTER these overlays; a digest computed without them would never be the
	// one a worker resolves. Both are idempotent, so applying them here is safe
	// for a caller that already did.
	instance.ApplyGaggleCICommand(set)
	instance.ApplyGaggleOutboxMirror(set)
	instructions, skillPackages, err := runtimewiring.LoadSnapshotGooberInputs(configDir, set)
	if err != nil {
		return nil, fmt.Errorf("startpin: load goober kit content: %w", err)
	}
	// Deferred model discovery and no credential or connector check: the same
	// policy runtimewiring.NewGooberDigestIndex applies for worker pins, which
	// is what makes these digests comparable to a worker's.
	machines, gooberDigests, _, _, err := runtimewiring.CompiledMachinesWithGooberDigests(
		set,
		runtimewiring.GoobersByName(set),
		instructions,
		runtimewiring.HarnessEnvironmentPolicy(cfg.Runner),
		cfg.Runner.HarnessCommand,
		true, nil, nil,
		func(gaggle string, _ map[string]apiv1.GooberSpec) (map[string][]workflow.SkillFile, error) {
			return skillPackages[gaggle], nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("startpin: compile generation: %w", err)
	}
	out := make(map[string]cloudconfig.Pins, len(machines))
	for identity, machine := range machines {
		digest, ok := gooberDigests[identity]
		if !ok {
			return nil, fmt.Errorf("startpin: no goober digest for %s/%s", identity.Gaggle, identity.Workflow)
		}
		out[identity.Gaggle+"/"+identity.Workflow] = cloudconfig.Pins{WorkflowDigest: machine.Digest(), GooberDigest: digest}
	}
	return out, nil
}
