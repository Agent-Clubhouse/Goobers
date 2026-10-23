package runtimewiring

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// PodCredentialResolver satisfies credentials.Resolver against the credential
// plane instead of the instance's credential stores.
//
// The plane is CAPABILITY-keyed while the resolver is asked for a credential
// REF, so the kit's grants provide the mapping: a ref names the credential a
// grant entitles, and the grant names the capability the plane resolves.
//
// ONE REF BACKS MANY CAPABILITIES, so this is deliberately ref -> []capability.
// credentials.RunnerGrants assigns the SAME default ref (the project repo's
// token) to every credentialed capability, so a map[string]string here keeps
// only the last grant written and every lookup returns that one capability.
// MEASURED: a stage declaring repo:push resolved to "ado:pr:complete" — the
// last element of credentialedCapabilities — and failed with that capability
// "not materialised", naming a capability the workflow never mentioned.
//
// A ref names ONE underlying credential, so any capability it backs that the
// plane did materialise yields the same token: the first hit is the answer.
type PodCredentialResolver struct {
	ByRef    map[string][]string // credential ref -> capabilities it backs
	Vals     map[string]string   // capability -> resolved value
	Expiries map[string]time.Time
}

// Resolve resolves a credential granted to the stage.
func (r PodCredentialResolver) Resolve(ctx context.Context, name string) (string, error) {
	value, _, err := r.ResolveWithExpiry(ctx, name)
	return value, err
}

// ResolveWithExpiry resolves a granted credential and its expiry.
func (r PodCredentialResolver) ResolveWithExpiry(ctx context.Context, name string) (string, time.Time, error) {
	if err := ctx.Err(); err != nil {
		return "", time.Time{}, err
	}
	capabilities, ok := r.ByRef[name]
	if !ok {
		return "", time.Time{}, fmt.Errorf("credential %q is not granted to this stage", name)
	}
	for _, capability := range capabilities {
		if value, ok := r.Vals[capability]; ok {
			return value, r.Expiries[capability], nil
		}
	}
	// The plane materialised none of them. Name every capability the ref backs
	// rather than an arbitrary one: the operator granted a capability, and
	// which of these is missing is exactly what they need to see.
	return "", time.Time{}, fmt.Errorf("credential %q is backed by capabilities %s, none of which were materialised by the credential plane",
		name, strings.Join(capabilities, ", "))
}

// PodHarnessRegistry builds the pod's harness registry, and is a var SO THAT
// buildPodAgenticExecutor HAS A TEST CALLER AT ALL.
//
// The real builder produces adapters that preflight an actual harness binary,
// which is why this constructor had none — and why the one invariant it cannot
// get wrong (the staging directory it is handed must be the directory the
// executor reads context from) was unobservable: a reviewer's ablation that
// reintroduced a constructor-local os.MkdirTemp here, the original bug's exact
// shape, passed the entire cmd/goobers suite. Swapping the registry is the
// smallest seam that lets a test drive the whole constructor. Mirrors the
// existing newAgenticAdapter / repoCloneURL test seams.
var PodHarnessRegistry = BuildHarnessRegistry

// PodHarnessEnvironment is the pod's view of the instance harness policy, as
// the daemon wrote it into the kit for the selected harness. A settle budget
// the kit does not carry, or cannot parse, keeps the adapter default: the
// daemon validated it at load, so only a legacy kit omits it.
func PodHarnessEnvironment(kit *agentickit.Kit, selected apiv1.Harness) harness.EnvironmentConfig {
	settle, _ := (instance.RunnerConfig{RequiredMCPSettleTimeout: kit.RequiredMCPSettleTimeout}).RequiredMCPSettleTimeoutDuration()
	return harness.EnvironmentConfig{
		Unset:                    kit.HarnessEnvUnset,
		SessionArgs:              map[string][]string{string(selected): kit.HarnessSessionArgs},
		PreflightArgs:            map[string][]string{string(selected): kit.HarnessPreflightArgs},
		RequiredMCPSettleTimeout: settle,
	}
}

// BuildPodAgenticExecutor constructs the executor from the kit plus the pod's
// own local facilities.
// runsDir is the staging root the caller already created and already
// materialized this stage's context into; it becomes both the recorder's Dir()
// and the contextResolver's root, which is what makes the two agree.
func BuildPodAgenticExecutor(kit *agentickit.Kit, stderr io.Writer, minted []dispatcher.MintedCredential, runsDir string, customize func(AgenticExecutorInput) AgenticExecutorInput) (invoke.Goober, error) {
	gooberName := kit.Envelope.Goober
	resolver := PodCredentialResolver{ByRef: map[string][]string{}, Vals: map[string]string{}, Expiries: map[string]time.Time{}}
	for _, g := range kit.Grants {
		if g.Ref != "" && g.Capability != "" {
			resolver.ByRef[g.Ref] = append(resolver.ByRef[g.Ref], g.Capability)
		}
	}
	registry, scrubber := journal.DefaultScrubber()
	registry.Register([]byte(os.Getenv(dispatcher.JournalTokenEnv)))
	for _, c := range minted {
		resolver.Vals[c.Capability] = c.Value
		if c.ExpiresAt != nil && !c.ExpiresAt.IsZero() {
			resolver.Expiries[c.Capability] = *c.ExpiresAt
		}
		// Register before use so the value is scrubbed out of transcripts and
		// journal events even if the harness echoes it.
		registry.Register([]byte(c.Value))
	}

	// APPLY THE CAPABILITY→ENV MAPPING the kit carries. The harness reads its
	// credential from the environment (agent:model -> COPILOT_GITHUB_TOKEN),
	// and on the worker that variable is ambient — supplied by the deployment.
	// A pod has no ambient credentials by design, so without this the harness
	// preflight fails before the stage ever starts:
	//
	//	harness "copilot" preflight failed in pod:
	//	  Error: No authentication information found.
	//
	// Sourcing it from the PLANE rather than a deployment secret is strictly
	// better than the worker's posture: the value is scoped to this run, and
	// no long-lived model credential sits in a pod spec or a mounted secret.
	// Every value is registered with the scrubber above before it is set here,
	// so a harness that echoes it into a transcript still cannot leak it.
	for _, c := range minted {
		envVar, ok := kit.EnvCapabilities[c.Capability]
		if !ok || envVar == "" || c.Value == "" {
			continue
		}
		// The same provider rule the harness applies to its subprocess
		// environment: the pod's own preflight and model discovery read these
		// variables too, so a repository credential is set only where its
		// provider's tooling reads it.
		if !harness.CredentialFitsEnvAudience(c.Capability, envVar, kit.Envelope.RepoRef.Provider) {
			continue
		}
		if err := os.Setenv(envVar, c.Value); err != nil {
			return nil, fmt.Errorf("apply credential for capability %s: %w", c.Capability, err)
		}
	}

	grants := make([]credentials.Grant, 0, len(kit.Grants))
	for _, g := range kit.Grants {
		grants = append(grants, credentials.Grant{Goober: g.Goober, Capability: g.Capability, Ref: g.Ref})
	}

	// The harness is PREFLIGHTED HERE, against the pod's own binary. Shipping
	// the worker's preflight result would assert something false: it describes
	// the worker's copilot, not this pod's.
	//
	// No modelCredential resolver: the minted credential was just applied to
	// the ambient environment above, so the preflight's ambient-env-first
	// lookup already finds it. There's no *instance.Config/StoreResolver in
	// this pod-context function to build one anyway.
	// No tmp:ephemeral binding either: in a stage pod the effect is enforced
	// BY CONSTRUCTION — a fresh, never-reused pod whose /tmp is the
	// dispatcher's sized emptyDir (internal/dispatcher/podspec.go
	// stampVolumes). The daemon-side binding exists precisely because runner
	// `self` has no such pod; layering it here would carve an ephemeral
	// directory inside an already-ephemeral one.
	spec, ok := kit.Goobers[gooberName]
	if !ok {
		return nil, fmt.Errorf("kit carries no spec for goober %q", gooberName)
	}
	var commands map[string][]string
	if len(kit.HarnessCommand) > 0 {
		commands = map[string][]string{string(spec.Harness): kit.HarnessCommand}
	}
	// The pod launches goobers-io through this binary. Passing an empty self
	// binary silently omits its MCP registration and leaves artifact-producing
	// agents unable to publish their declared output.
	selfBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve goobers binary path in pod: %w", err)
	}
	adapterRegistry, err := PodHarnessRegistry(kit.EnvCapabilities, PodHarnessEnvironment(kit, spec.Harness), commands, "", selfBin, false, nil, false)
	if err != nil {
		return nil, fmt.Errorf("build harness registry: %w", err)
	}
	// Preflight THIS goober's harness specifically, rather than walking
	// workflows as the daemon does — a pod has no workflow set, and the only
	// harness that matters here is the one this stage is about to use.
	adapter, err := adapterRegistry.Get(string(spec.Harness))
	if err != nil {
		return nil, fmt.Errorf("harness %q is not available in this pod: %w", spec.Harness, err)
	}
	info, err := PreflightAdapterConfig(context.Background(), adapter, spec)
	if err != nil {
		// Fail closed with the harness's own message: an unusable harness
		// discovered mid-invocation is a burned attempt with the cause buried
		// in a transcript.
		return nil, fmt.Errorf("harness %q preflight failed in pod: %w", spec.Harness, err)
	}
	harnessInfo := HarnessPreflightInfo{spec.Harness: info}
	for _, warning := range info.Warnings {
		_, _ = fmt.Fprintf(stderr, "HARNESS %s WARNING: %s\n", spec.Harness, warning)
	}

	input := PodAgenticExecutorInput(PodExecutorWiring{
		Kit:             kit,
		RunsDir:         runsDir,
		Stderr:          stderr,
		Scrubber:        scrubber,
		Registry:        registry,
		Resolver:        resolver,
		Grants:          grants,
		AdapterRegistry: adapterRegistry,
		HarnessInfo:     harnessInfo,
	})
	if customize != nil {
		input = customize(input)
	}
	return BuildAgenticExecutor(input)
}

// PodExecutorWiring is everything buildPodAgenticExecutor discovers about this
// pod before it can name an executor: the kit it fetched, the staging root its
// caller materialized context into, and the local facilities (scrubber,
// credential resolver, preflighted harness) it built along the way.
type PodExecutorWiring struct {
	Kit     *agentickit.Kit
	RunsDir string
	Stderr  io.Writer
	// Scrubber redacts artifact bytes before they are digested and emitted.
	Scrubber journal.Scrubber
	// Registry is the same scrubber's registry, used as both the shared secret
	// registry and the SecretRegistrar.
	Registry        *journal.RegistryScrubber
	Resolver        credentials.Resolver
	Grants          []credentials.Grant
	AdapterRegistry *harness.Registry
	HarnessInfo     HarnessPreflightInfo
}

// PodAgenticExecutorInput assembles the executor input for a pod stage.
// Daemon-host credential paths are deliberately absent: the kit carries scoped
// credentials, not the daemon config or its credential-file mounts. Deployment
// isolation must keep those host files out of the pod.
//
// FACTORED OUT SO THE DIRECTORY AGREEMENT IS OBSERVABLE. The bug this file's
// change fixes crosses two edges: materializePodContext must be CALLED before
// the executor is built, AND it must fill the SAME directory the executor's
// contextResolver reads. The first edge is pinned by a test that drives
// runAgenticStage. The second could not be pinned at the production
// constructor, because buildPodAgenticExecutor calls adapter.Preflight against
// a real harness binary and therefore has no test callers at all — so a
// refactor that reintroduced a constructor-local staging dir (the original
// bug's exact shape) would ship green.
//
// RunsDir and the recorder's dir are the two fields that MUST name that one
// directory: the first is where harness.NewContextResolver looks for an
// upstream artifact, the second is where a produced artifact is reported from.
// Both are assigned from w.RunsDir here, in a function a test can call, and
// TestPodAgenticStageReadsAnUpstreamArtifactPointer builds its executor through
// this function rather than a hand-assembled replica — so the agreement is
// exercised by the same code production runs.
func PodAgenticExecutorInput(w PodExecutorWiring) AgenticExecutorInput {
	return AgenticExecutorInput{
		GooberName:      w.Kit.Envelope.Goober,
		Goobers:         w.Kit.Goobers,
		Instructions:    w.Kit.Instructions,
		SkillPackages:   w.Kit.SkillPackages,
		Assets:          w.Kit.AssetBundles(),
		HarnessInfo:     w.HarnessInfo,
		AdapterRegistry: w.AdapterRegistry,
		EnvCapabilities: w.Kit.EnvCapabilities,
		Resolver:        w.Resolver,
		Grants:          w.Grants,
		SharedRegistry:  w.Registry,
		RunsDir:         w.RunsDir,
		SandboxPosture:  instance.SandboxPosture(w.Kit.SandboxPosture),

		SecretRegistrar: w.Registry,

		AgenticAdapter: NewAgenticAdapter,
	}
}
