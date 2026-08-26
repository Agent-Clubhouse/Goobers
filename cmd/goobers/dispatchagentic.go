package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// dispatchagentic.go is the pod half of the agentic claim check.
//
// It fetches the stage's kit by digest, VERIFIES it, and builds the executor
// with buildAgenticExecutor — the SAME constructor the worker uses. That is
// deliberate: an agentic stage that ran through a second, pod-only construction
// path could diverge from the local one in what instructions or tools it
// applied, and the divergence would show up as a differently-behaved agent
// rather than as an error.

// runAgenticStage executes an agentic stage inside a pod and returns the
// envelope to surrender.
func runAgenticStage(ctx context.Context, stdout, stderr io.Writer) apiv1.ResultEnvelope {
	digest := strings.TrimSpace(os.Getenv(dispatcher.EnvAgenticKitDigest))
	if digest == "" {
		return failureEnvelope("agentic_kit_missing", "no agentic kit digest was stamped on this pod")
	}
	kit, err := fetchAgenticKit(ctx, digest)
	if err != nil {
		// Fail closed and name the kit. A stage that proceeded without its kit
		// would run with no instructions at all.
		return failureEnvelope("agentic_kit_unavailable", err.Error())
	}

	exec, err := buildPodAgenticExecutor(ctx, kit, stderr)
	if err != nil {
		return failureEnvelope("agentic_executor_unavailable", err.Error())
	}

	result, err := exec.Invoke(ctx, kit.Envelope)
	if err != nil {
		return failureEnvelope("agentic_invocation_failed", err.Error())
	}
	return result
}

// fetchAgenticKit reads the kit from the blob plane and verifies it against the
// digest the dispatcher stamped.
func fetchAgenticKit(ctx context.Context, digest string) (*agentickit.Kit, error) {
	endpoint := strings.TrimSpace(os.Getenv(dispatcher.EnvBlobEndpoint))
	if endpoint == "" {
		return nil, fmt.Errorf("no blob endpoint is configured for this pod")
	}
	client := &dispatcher.BlobClient{BaseURL: endpoint, Token: os.Getenv(dispatcher.EnvPodToken)}
	data, err := client.Get(ctx, digest)
	if err != nil {
		return nil, fmt.Errorf("fetch kit %s: %w", digest, err)
	}
	// Unmarshal verifies the content address. A kit whose bytes do not hash to
	// the digest we were told to expect is REFUSED rather than executed:
	// running a substituted kit means running this stage under different
	// instructions, which fails silently and looks like a misbehaving agent.
	kit, err := agentickit.Unmarshal(data, digest)
	if err != nil {
		return nil, err
	}
	return kit, nil
}

// podCredentialResolver satisfies credentials.Resolver against the credential
// plane instead of the instance's credential stores.
//
// The plane is CAPABILITY-keyed while the resolver is asked for a credential
// REF, so the kit's grants provide the mapping: a ref names the credential a
// grant entitles, and the grant names the capability the plane resolves.
type podCredentialResolver struct {
	byRef map[string]string // credential ref -> capability
	vals  map[string]string // capability -> resolved value
}

func (r podCredentialResolver) Resolve(_ context.Context, name string) (string, error) {
	capability, ok := r.byRef[name]
	if !ok {
		return "", fmt.Errorf("credential %q is not granted to this stage", name)
	}
	value, ok := r.vals[capability]
	if !ok {
		// The plane did not materialise it. Say which capability, not just
		// which ref, because the capability is what the operator granted.
		return "", fmt.Errorf("capability %q was not materialised by the credential plane", capability)
	}
	return value, nil
}

// buildPodAgenticExecutor constructs the executor from the kit plus the pod's
// own local facilities.
func buildPodAgenticExecutor(ctx context.Context, kit *agentickit.Kit, stderr io.Writer) (invoke.Goober, error) {
	gooberName := kit.Envelope.Goober

	// Credentials come from the plane, resolved at stage start exactly as a
	// deterministic stage resolves its own — no credential material ever rode
	// the kit.
	minted, err := resolveStageCredentials(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve stage credentials: %w", err)
	}
	resolver := podCredentialResolver{byRef: map[string]string{}, vals: map[string]string{}}
	for _, g := range kit.Grants {
		if g.Ref != "" {
			resolver.byRef[g.Ref] = g.Capability
		}
	}
	registry, scrubber := journal.DefaultScrubber()
	for _, c := range minted {
		resolver.vals[c.Capability] = c.Value
		// Register before use so the value is scrubbed out of transcripts and
		// journal events even if the harness echoes it.
		registry.Register([]byte(c.Value))
	}

	grants := make([]credentials.Grant, 0, len(kit.Grants))
	for _, g := range kit.Grants {
		grants = append(grants, credentials.Grant{Goober: g.Goober, Capability: g.Capability, Ref: g.Ref})
	}

	// The harness is PREFLIGHTED HERE, against the pod's own binary. Shipping
	// the worker's preflight result would assert something false: it describes
	// the worker's copilot, not this pod's.
	adapterRegistry, err := buildHarnessRegistry(kit.EnvCapabilities, nil, nil, "", "", false)
	if err != nil {
		return nil, fmt.Errorf("build harness registry: %w", err)
	}
	// Preflight THIS goober's harness specifically, rather than walking
	// workflows as the daemon does — a pod has no workflow set, and the only
	// harness that matters here is the one this stage is about to use.
	spec, ok := kit.Goobers[gooberName]
	if !ok {
		return nil, fmt.Errorf("kit carries no spec for goober %q", gooberName)
	}
	adapter, err := adapterRegistry.Get(string(spec.Harness))
	if err != nil {
		return nil, fmt.Errorf("harness %q is not available in this pod: %w", spec.Harness, err)
	}
	info, err := adapter.Preflight(ctx)
	if err != nil {
		// Fail closed with the harness's own message: an unusable harness
		// discovered mid-invocation is a burned attempt with the cause buried
		// in a transcript.
		return nil, fmt.Errorf("harness %q preflight failed in pod: %w", spec.Harness, err)
	}
	harnessInfo := harnessPreflightInfo{spec.Harness: info}

	runsDir, err := os.MkdirTemp("", "goobers-agentic-runs-*")
	if err != nil {
		return nil, fmt.Errorf("create runs dir: %w", err)
	}

	return buildAgenticExecutor(agenticExecutorInput{
		GooberName:       gooberName,
		Goobers:          kit.Goobers,
		Instructions:     kit.Instructions,
		Assets:           kit.AssetBundles(),
		HarnessInfo:      harnessInfo,
		AdapterRegistry:  adapterRegistry,
		EnvCapabilities:  kit.EnvCapabilities,
		Resolver:         resolver,
		Grants:           grants,
		SharedRegistry:   registry,
		RunsDir:          runsDir,
		SandboxPosture:   instance.SandboxPosture(kit.SandboxPosture),
		ArtifactRecorder: podArtifactRecorder{stderr: stderr, scrubber: scrubber},
		SecretRegistrar:  registry,
		AgenticAdapter:   newAgenticAdapter,
	})
}

// podArtifactRecorder satisfies runner.ArtifactRecorder inside a stage pod.
//
// A pod has no run journal on disk, so artifacts are emitted through the
// journal plane and their content address is DERIVED — journal.ArtifactRef
// computes the exact Ref the daemon's writer produces for the same bytes, which
// is the same derivation the deterministic path already relies on.
type podArtifactRecorder struct {
	stderr   io.Writer
	scrubber journal.Scrubber
}

func (r podArtifactRecorder) RecordArtifact(name string, data []byte) (journal.Ref, error) {
	scrubbed := data
	if r.scrubber != nil {
		scrubbed = r.scrubber.Scrub(data)
	}
	ref, err := journal.ArtifactRef(scrubbed)
	if err != nil {
		return journal.Ref{}, fmt.Errorf("derive artifact ref for %s: %w", name, err)
	}
	// Best effort, exactly as the deterministic path treats stream artifacts:
	// the stage has already produced its result, and losing an artifact must
	// not turn a completed invocation into a failure.
	recordStageArtifacts(context.Background(), r.stderr, map[string][]byte{name: scrubbed})
	return ref, nil
}
