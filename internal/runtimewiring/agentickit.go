package runtimewiring

import (
	"fmt"
	"path/filepath"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/workflow"
)

// BuildAgenticKit assembles only the selected goober from a captured configuration snapshot.
func BuildAgenticKit(l instance.Layout, cfg *instance.Config, set *instance.ConfigSet, capturedInstructions map[string]string, skillPackages map[string]map[string][]workflow.SkillFile, env apiv1.InvocationEnvelope, mode agentickit.Mode, registrar *journal.RegistryScrubber) (*agentickit.Kit, error) {

	goobers, err := ResolveGoobersForGaggle(set, env.Gaggle)
	if err != nil {
		return nil, err
	}
	// SCOPED TO THE ONE GOOBER THIS STAGE RUNS AS. A kit carrying every
	// goober's instructions would hand a stage the definitions of agents it has
	// no business seeing, and would grow with the gaggle rather than with the
	// stage.
	spec, ok := goobers[env.Goober]
	if !ok {
		return nil, fmt.Errorf("goober %q is not defined in gaggle %q", env.Goober, env.Gaggle)
	}
	spec = MaterializeAgenticKitGoober(set, env, spec)
	scoped := map[string]apiv1.GooberSpec{env.Goober: spec}

	instructions, err := scopedInstructions(capturedInstructions, scoped)
	if err != nil {
		return nil, fmt.Errorf("load goober instructions: %w", err)
	}

	assets := make(map[string]*gooberassets.WireBundle, 1)
	bundle, err := gooberassets.Load(filepath.Join(ResolvedGooberDefinitionDir(l.ConfigDir(), set, spec, env.Goober), gooberassets.SourceDir))
	if err != nil {
		return nil, fmt.Errorf("load goober %q assets: %w", env.Goober, err)
	}
	assets[env.Goober] = bundle.ToWire()

	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return nil, fmt.Errorf("resolve credential stores: %w", err)
	}
	project := GaggleProjectRef(set, env.Gaggle)
	_, grants, err := BuildRoleCredentials(cfg, stores, project.Owner, project.Name, GaggleBacklogRole(project, GaggleBacklogRef(set, env.Gaggle)), nil, registrar)
	if err != nil {
		return nil, fmt.Errorf("derive credential grants: %w", err)
	}
	// Resolve harness scoping BEFORE the claim check crosses into the pod.
	// Besides making the plane's plain `agent:model` response line up with the
	// kit's capability->ref map, this is the least-authority shape: a Copilot
	// pod never receives even the reference for a Claude Code credential (or
	// vice versa). buildGooberCredentialGrants is deliberately idempotent, so
	// the shared executor constructor can safely scope this already-bound set
	// once more in the pod.
	harnessName := spec.Harness
	if harnessName == "" {
		harnessName = apiv1.HarnessCopilot
	}
	credentialKeys := append([]string(nil), spec.Capabilities...)
	credentialKeys = append(credentialKeys, mcpconfig.BYOCredentialKeys(spec.MCPServers)...)
	selectedGrants := BuildGooberCredentialGrants(env.Goober, string(harnessName), credentialKeys, grants)
	wireGrants := make([]agentickit.Grant, 0, len(selectedGrants))
	for _, g := range selectedGrants {
		// Shape only — Ref names where a credential lives, never its value.
		wireGrants = append(wireGrants, agentickit.Grant{Goober: g.Goober, Capability: g.Capability, Ref: g.Ref})
	}
	envCapabilities := BuildEnvCapabilities()
	if spec.Harness == apiv1.HarnessCodex {
		envCapabilities[string(capability.AgentModel)] = CodexModelEnv
	}

	return &agentickit.Kit{
		Envelope:             env,
		Mode:                 mode,
		Goobers:              scoped,
		Instructions:         instructions,
		SkillPackages:        workflow.ResolvedSkillFiles(spec, skillPackages[env.Gaggle]),
		Assets:               assets,
		EnvCapabilities:      envCapabilities,
		Grants:               wireGrants,
		SandboxPosture:       string(instance.EffectiveAgenticSandbox(cfg, nil)),
		HarnessCommand:       slices.Clone(cfg.Runner.HarnessCommand[string(spec.Harness)]),
		HarnessEnvUnset:      slices.Clone(cfg.Runner.HarnessEnvUnset),
		HarnessSessionArgs:   slices.Clone(cfg.Runner.HarnessSessionArgs[string(spec.Harness)]),
		HarnessPreflightArgs: slices.Clone(cfg.Runner.HarnessPreflightArgs[string(spec.Harness)]),

		RequiredMCPSettleTimeout: cfg.Runner.RequiredMCPSettleTimeout,
	}, nil
}

// MaterializeAgenticKitGoober resolves one goober and its captured inputs into a portable kit.
func MaterializeAgenticKitGoober(set *instance.ConfigSet, env apiv1.InvocationEnvelope, spec apiv1.GooberSpec) apiv1.GooberSpec {
	for i := range set.Workflows {
		candidate := &set.Workflows[i]
		if candidate.Name == env.WorkflowID && candidate.Spec.Gaggle == env.Gaggle {
			return workflow.MaterializeGooberForDefinition(workflow.Definition{
				Name: candidate.Name, DSLVersion: candidate.DSLVersion, Spec: candidate.Spec,
			}, spec)
		}
	}
	return spec
}

func scopedInstructions(captured map[string]string, goobers map[string]apiv1.GooberSpec) (map[string]string, error) {
	out := make(map[string]string, len(goobers))
	for name := range goobers {
		content, ok := captured[name]
		if !ok {
			return nil, fmt.Errorf("config snapshot carries no instructions for goober %q", name)
		}
		out[name] = content
	}
	return out, nil
}
