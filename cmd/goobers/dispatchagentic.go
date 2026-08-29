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
	"github.com/goobers/goobers/internal/livejournal"
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

	// An agentic stage needs its workspace PROVISIONED and STAMPED, exactly as
	// the local path does — provisionWorkspace mutates the envelope so the
	// harness knows where to work. Missing either half fails inside the
	// harness with nothing about workspaces in the message:
	//
	//	harness: copilot-cli: RunRequest.Workspace is empty
	//
	// The credential is resolved first because the checkout authenticates with
	// it, and resolving twice would mint two credentials for one stage.
	minted, err := resolveStageCredentials(ctx)
	if err != nil {
		return failureEnvelope("credential_resolve_failed", err.Error())
	}
	workspace, err := os.Getwd()
	if err != nil {
		return failureEnvelope("workspace_provision_failed", fmt.Sprintf("resolve workspace: %v", err))
	}
	// The checkout may use a credential the AGENT never receives (#3770): it
	// provisions the working tree and is excluded from buildPodAgenticExecutor
	// below, so the goober's resolver and its environment see only what the
	// stage actually declared.
	checkoutCreds, checkoutErr := resolveCheckoutCredential(ctx)
	if checkoutErr != nil {
		return failureEnvelope("credential_resolve_failed", checkoutErr.Error())
	}
	if err := checkoutRepoWorkspace(ctx, workspace, stderr, append(append([]dispatcher.MintedCredential{}, minted...), checkoutCreds...)); err != nil {
		return failureEnvelope("workspace_provision_failed", err.Error())
	}
	// The stamp the harness actually reads.
	kit.Envelope.Workspace = workspace

	exec, err := buildPodAgenticExecutor(kit, stderr, minted)
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
type podCredentialResolver struct {
	byRef map[string][]string // credential ref -> capabilities it backs
	vals  map[string]string   // capability -> resolved value
}

func (r podCredentialResolver) Resolve(_ context.Context, name string) (string, error) {
	capabilities, ok := r.byRef[name]
	if !ok {
		return "", fmt.Errorf("credential %q is not granted to this stage", name)
	}
	for _, capability := range capabilities {
		if value, ok := r.vals[capability]; ok {
			return value, nil
		}
	}
	// The plane materialised none of them. Name every capability the ref backs
	// rather than an arbitrary one: the operator granted a capability, and
	// which of these is missing is exactly what they need to see.
	return "", fmt.Errorf("credential %q is backed by capabilities %s, none of which were materialised by the credential plane",
		name, strings.Join(capabilities, ", "))
}

// buildPodAgenticExecutor constructs the executor from the kit plus the pod's
// own local facilities.
func buildPodAgenticExecutor(kit *agentickit.Kit, stderr io.Writer, minted []dispatcher.MintedCredential) (invoke.Goober, error) {
	gooberName := kit.Envelope.Goober
	resolver := podCredentialResolver{byRef: map[string][]string{}, vals: map[string]string{}}
	for _, g := range kit.Grants {
		if g.Ref != "" && g.Capability != "" {
			resolver.byRef[g.Ref] = append(resolver.byRef[g.Ref], g.Capability)
		}
	}
	registry, scrubber := journal.DefaultScrubber()
	for _, c := range minted {
		resolver.vals[c.Capability] = c.Value
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
	adapterRegistry, err := buildHarnessRegistry(kit.EnvCapabilities, nil, nil, "", "", false, nil)
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
	info, err := adapter.Preflight(context.Background())
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
		ArtifactRecorder: podArtifactRecorder{stderr: stderr, scrubber: scrubber, dir: runsDir},
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
// It must satisfy every interface buildAgenticExecutor type-asserts on the
// recorder, because those assertions fail at CONSTRUCTION rather than at first
// use — so a missing method is a stage that never starts, reported as
// "recorder does not implement X" with no other context.
//
// The full set, swept from the construction path rather than discovered one
// failed run at a time: harness.SpanRecorder, harness.ArtifactRecorder,
// interface{ Dir() string }, and (for the external-telemetry path)
// executor.BoundedArtifactRecorder. The SecretRegistrar must separately be a
// journal.Scrubber, which *journal.RegistryScrubber already is.
type podArtifactRecorder struct {
	stderr   io.Writer
	scrubber journal.Scrubber
	dir      string
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

// RecordSpanWithSchema satisfies harness.SpanRecorder. A span is an artifact
// under a "spans" prefix — the same shape the worker-side recorder uses, so a
// span produced in a pod lands under the same name it would locally.
func (r podArtifactRecorder) RecordSpanWithSchema(stage, name, dataSchema string, data []byte) (journal.Ref, error) {
	_ = stage
	_ = dataSchema
	return r.RecordArtifact("spans/"+name, data)
}

// RecordArtifactBounded satisfies executor.BoundedArtifactRecorder. The limit
// is applied AFTER scrubbing, at the same boundary that digests the bytes, so
// a truncated artifact still has a digest committing to what was stored.
func (r podArtifactRecorder) RecordArtifactBounded(name string, data []byte, maxBytes int) (journal.Ref, error) {
	return r.RecordArtifactBoundedWithIntegrity(name, data, apiv1.IntegrityDerived, maxBytes)
}

// RecordArtifactBoundedWithIntegrity is RecordArtifactBounded with an explicit
// provenance grade.
func (r podArtifactRecorder) RecordArtifactBoundedWithIntegrity(name string, data []byte, _ apiv1.Integrity, maxBytes int) (journal.Ref, error) {
	scrubbed := data
	if r.scrubber != nil {
		scrubbed = r.scrubber.Scrub(data)
	}
	if maxBytes > 0 && len(scrubbed) > maxBytes {
		scrubbed = scrubbed[:maxBytes]
	}
	return r.RecordArtifact(name, scrubbed)
}

// Dir satisfies the interface{ Dir() string } the executor asserts for its
// staging directory.
func (r podArtifactRecorder) Dir() string { return r.dir }

// Append satisfies harness.EventAppender, which the executor asserts at
// INVOCATION time — not construction — in four places: sandbox posture,
// structured agent telemetry, and two others. A recorder without it fails
// mid-invocation with
//
//	structured agent telemetry requires a journal-backed recorder;
//	main.podArtifactRecorder cannot append events
//
// A pod has no run journal on disk, so the event goes through the journal
// plane — the same route artifacts take, and the same route the worker's live
// journal uses. This is what makes an agentic stage's telemetry land in the
// run's journal at all rather than being dropped because the pod is not the
// daemon.
func (r podArtifactRecorder) Append(ev journal.Event) error {
	daemonAPI := strings.TrimSpace(os.Getenv(dispatcher.EnvDaemonAPI))
	runID := os.Getenv(dispatcher.EnvRunID)
	if daemonAPI == "" || runID == "" {
		// No plane configured: the loopback/no-journal posture. Dropping the
		// event is correct here and must NOT fail the stage — the agent's work
		// is the outcome, its telemetry is not.
		return nil
	}
	emitter := &livejournal.HTTPEmitter{BaseURL: daemonAPI, Token: os.Getenv(dispatcher.EnvPodToken)}
	event := ev
	_, err := emitter.Emit(context.Background(), livejournal.EmitRequest{
		RunID:  runID,
		Gaggle: os.Getenv(dispatcher.EnvGaggle),
		Ops: []livejournal.Op{{
			Kind:  livejournal.OpAppend,
			Key:   fmt.Sprintf("%s/%s/%d", os.Getenv(dispatcher.EnvStage), ev.Type, ev.Seq),
			Event: &event,
		}},
	})
	return err
}
