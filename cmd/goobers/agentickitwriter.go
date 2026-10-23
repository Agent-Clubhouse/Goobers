package main

import (
	"fmt"
	"path/filepath"
	"slices"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/workflow"
)

// agentickitwriter.go is the worker half of the agentic claim check.
//
// The worker has the instance configuration; a stage pod does not, and must
// not — that is the same property that stops a stage reading the fleet's
// config. So the worker resolves ONE goober's execution inputs, publishes them
// to the blob plane the pod already reaches, and the dispatcher stamps only the
// resulting digest.
//
// It re-derives from the instance root rather than extending buildRunnerConfig's
// return signature: that function is shared with the daemon, and widening a
// shared constructor to serve one caller is how the two paths start to drift.

// agenticKitWriter satisfies dispatcher.KitWriter.
type agenticKitWriter struct {
	registrar *journal.RegistryScrubber
}

// kitModeFor is the completion contract stamped INTO the kit (decision 001
// rulings 7–8): a reviewer gate attempt (Attempt.Review) owes a verdict,
// everything else a result. The pod learns which from the same verified,
// content-addressed document that carries its instructions — never from a
// pod-spec variable anything with namespace read could set.
func kitModeFor(attempt dispatcher.Attempt) agentickit.Mode {
	if attempt.Review {
		return agentickit.ModeReview
	}
	return agentickit.ModeInvoke
}

func (w agenticKitWriter) buildSnapshotKit(l instance.Layout, snapshot *workerConfigSnapshot, env apiv1.InvocationEnvelope, mode agentickit.Mode) (*agentickit.Kit, error) {
	if snapshot.configDir != "" {
		l = l.WithConfigDir(snapshot.configDir)
	}
	cfg, set := snapshot.cfg, snapshot.set

	goobers, err := resolveGoobersForGaggle(set, env.Gaggle)
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
	spec = materializeAgenticKitGoober(set, env, spec)
	scoped := map[string]apiv1.GooberSpec{env.Goober: spec}

	instructions, err := snapshot.instructionsFor(scoped)
	if err != nil {
		return nil, fmt.Errorf("load goober instructions: %w", err)
	}

	assets := make(map[string]*gooberassets.WireBundle, 1)
	bundle, err := gooberassets.Load(filepath.Join(resolvedGooberDefinitionDir(l.ConfigDir(), set, spec, env.Goober), gooberassets.SourceDir))
	if err != nil {
		return nil, fmt.Errorf("load goober %q assets: %w", env.Goober, err)
	}
	assets[env.Goober] = bundle.ToWire()

	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return nil, fmt.Errorf("resolve credential stores: %w", err)
	}
	project := gaggleProjectRef(set, env.Gaggle)
	_, grants, err := buildRoleCredentials(cfg, stores, project.Owner, project.Name, gaggleBacklogRole(project, gaggleBacklogRef(set, env.Gaggle)), nil, w.registrar)
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
	selectedGrants := buildGooberCredentialGrants(env.Goober, string(harnessName), credentialKeys, grants)
	wireGrants := make([]agentickit.Grant, 0, len(selectedGrants))
	for _, g := range selectedGrants {
		// Shape only — Ref names where a credential lives, never its value.
		wireGrants = append(wireGrants, agentickit.Grant{Goober: g.Goober, Capability: g.Capability, Ref: g.Ref})
	}
	envCapabilities := buildEnvCapabilities()
	if spec.Harness == apiv1.HarnessCodex {
		envCapabilities[string(capability.AgentModel)] = codexModelEnv
	}

	return &agentickit.Kit{
		Envelope:             env,
		Mode:                 mode,
		Goobers:              scoped,
		Instructions:         instructions,
		SkillPackages:        workflow.ResolvedSkillFiles(spec, snapshot.skillPackages[env.Gaggle]),
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

func materializeAgenticKitGoober(set *instance.ConfigSet, env apiv1.InvocationEnvelope, spec apiv1.GooberSpec) apiv1.GooberSpec {
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
