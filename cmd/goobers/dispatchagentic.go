package main

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

// runAgenticStage executes an agentic stage inside a pod and returns what it
// owes the surrender plane: the envelope for an invocation, the envelope plus
// the Verdict for a review (agentickit.ModeReview — decision 001 rulings 7–8).
func runAgenticStage(ctx context.Context, stdout, stderr io.Writer) stageOutcome {
	digest := strings.TrimSpace(os.Getenv(dispatcher.EnvAgenticKitDigest))
	if digest == "" {
		return stageOutcome{Result: failureEnvelope("agentic_kit_missing", "no agentic kit digest was stamped on this pod")}
	}
	kit, err := fetchAgenticKit(ctx, digest)
	if err != nil {
		// Fail closed and name the kit. A stage that proceeded without its kit
		// would run with no instructions at all.
		return stageOutcome{Result: failureEnvelope("agentic_kit_unavailable", err.Error())}
	}
	// fail shapes every refusal past this point. For a REVIEW the class
	// matters in a way it does not for an invocation: a task's surrendered
	// failure is a business outcome the workflow branches on, but a gate has
	// no branch for "the pod could not start", so the engine reads
	// Retryable as the pod's own infra/policy classification and retries a
	// substrate fault on a fresh pod under the gate's evaluator retry bound
	// (engine.reviewActivityResult). A verdict failure stays policy-classed
	// and fails the run; a harness failure carries the harness's own class
	// (see the review arm below), as the self arm's ReviewGoober does.
	fail := func(code, message string) stageOutcome {
		envelope := failureEnvelope(code, message)
		if kit.IsReview() && reviewSubstrateFailure(code) {
			envelope.Error.Retryable = true
		}
		return stageOutcome{Result: envelope}
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
		return fail("credential_resolve_failed", err.Error())
	}
	workspace, err := os.Getwd()
	if err != nil {
		return fail("workspace_provision_failed", fmt.Sprintf("resolve workspace: %v", err))
	}
	// The checkout may use a credential the AGENT never receives (#3770): it
	// provisions the working tree and is excluded from buildPodAgenticExecutor
	// below, so the goober's resolver and its environment see only what the
	// stage actually declared.
	checkoutCreds, checkoutErr := resolveCheckoutCredential(ctx)
	if checkoutErr != nil {
		return fail("credential_resolve_failed", checkoutErr.Error())
	}
	if err := checkoutRepoWorkspace(ctx, workspace, stderr, append(append([]dispatcher.MintedCredential{}, minted...), checkoutCreds...)); err != nil {
		return fail("workspace_provision_failed", err.Error())
	}
	// The stamp the harness actually reads.
	kit.Envelope.Workspace = workspace

	// The staging root: what the executor's contextResolver is rooted at AND
	// what the recorder reports as its Dir(). Created HERE rather than inside
	// buildPodAgenticExecutor because context materialization has to fill the
	// same directory the resolver will read, and a directory the constructor
	// alone knows about cannot be filled from out here.
	runsDir, err := os.MkdirTemp("", "goobers-agentic-runs-*")
	if err != nil {
		return fail("workspace_provision_failed", fmt.Sprintf("create runs dir: %v", err))
	}

	// Fetch this stage's upstream artifacts BEFORE the harness looks for them.
	// See dispatchcontext.go: without this every artifact-backed pointer fails
	// to resolve, because a pod's staging root starts empty.
	if err := materializePodContext(ctx, runsDir, kit.Envelope, stderr); err != nil {
		return fail("context_materialize_failed", err.Error())
	}

	if kit.IsReview() {
		// The reviewer's diff evidence (#301 parity on the pod path; decision
		// 001 ruling 7): computed HERE, by this binary, from the checkout the
		// delta was just applied to — never reported by a model — and handed
		// to the reviewer as the "<gate>.diff" context pointer exactly as the
		// local runner's recordReviewerDiff does. It has to run AFTER the
		// checkout (so the diff sees the subject stage's commits) and BEFORE
		// the harness resolves the envelope's pointers, and it fails closed:
		// a reviewer judging without the evidence it was promised produces a
		// confident verdict about the wrong change.
		//
		// The checkout credential is registered with the diff's scrubber even
		// though the AGENT never sees it: a commit could have captured it, and
		// the diff is journaled.
		pointer, derr := recordPodReviewerDiff(ctx, workspace, runsDir, os.Getenv(dispatcher.EnvStage),
			append(append([]dispatcher.MintedCredential{}, minted...), checkoutCreds...), stderr)
		if derr != nil {
			return fail("reviewer_diff_failed", derr.Error())
		}
		if pointer != nil {
			kit.Envelope.ContextPointers = append(kit.Envelope.ContextPointers, *pointer)
		}
	}

	exec, err := buildPodAgenticExecutor(kit, stderr, minted, runsDir)
	if err != nil {
		return fail("agentic_executor_unavailable", err.Error())
	}

	if kit.IsReview() {
		// The SAME executor and the SAME constructor as an invocation — only
		// the completion contract differs: harness.Executor.Review drives
		// ModeReview and validates the verdict against the shared schema.
		// The surrendered Result carries a bare success: the verdict IS the
		// outcome, and the engine routes on it after its own re-validation.
		verdict, err := exec.Review(ctx, kit.Envelope)
		if err != nil {
			// The harness's own class survives the surrender plane only as
			// Retryable, so it is committed HERE, where the marker is still
			// visible: harness.Executor.Review marks a session that ended
			// without a completion (ErrNoCompletion) as an
			// invoke.InfrastructureFailure, and the self arm's ReviewGoober
			// hands exactly that class to classifySeamError, so the gate's
			// evaluator retry bound covers it there. A pod that dropped the
			// class would fail the run where the worker would retry. A
			// verdict the schema refused, or a harness that would not run,
			// carries no marker and stays the review's own outcome.
			outcome := fail("agentic_review_failed", err.Error())
			outcome.Result.Error.Retryable = invoke.IsInfrastructureFailure(err)
			return outcome
		}
		return stageOutcome{
			Result:  apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "reviewer verdict surrendered"},
			Verdict: &verdict,
		}
	}

	result, err := exec.Invoke(ctx, kit.Envelope)
	if err != nil {
		return fail("agentic_invocation_failed", err.Error())
	}
	return stageOutcome{Result: result}
}

// reviewSubstrateFailure names the pod-side refusals that are faults of the
// SUBSTRATE — the credential plane, the checkout, the blob plane, the runner
// image — rather than of the review itself. A review that fails here never
// reached its harness, so a fresh pod may well succeed; the engine reads the
// Retryable marking this drives and retries under the gate's evaluator
// retry bound. Everything else (a harness that would not run, a verdict
// the schema refused, the diff evidence failing) is the review's own
// outcome and fails the run.
func reviewSubstrateFailure(code string) bool {
	switch code {
	case "credential_resolve_failed", "workspace_provision_failed", "context_materialize_failed", "agentic_executor_unavailable":
		return true
	}
	return false
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

// podHarnessRegistry builds the pod's harness registry, and is a var SO THAT
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
var podHarnessRegistry = buildHarnessRegistry

// buildPodAgenticExecutor constructs the executor from the kit plus the pod's
// own local facilities.
// runsDir is the staging root the caller already created and already
// materialized this stage's context into; it becomes both the recorder's Dir()
// and the contextResolver's root, which is what makes the two agree.
func buildPodAgenticExecutor(kit *agentickit.Kit, stderr io.Writer, minted []dispatcher.MintedCredential, runsDir string) (invoke.Goober, error) {
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
	adapterRegistry, err := podHarnessRegistry(kit.EnvCapabilities, nil, nil, "", "", false, nil)
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

	return buildAgenticExecutor(podAgenticExecutorInput(podExecutorWiring{
		Kit:             kit,
		RunsDir:         runsDir,
		Stderr:          stderr,
		Scrubber:        scrubber,
		Registry:        registry,
		Resolver:        resolver,
		Grants:          grants,
		AdapterRegistry: adapterRegistry,
		HarnessInfo:     harnessInfo,
	}))
}

// podExecutorWiring is everything buildPodAgenticExecutor discovers about this
// pod before it can name an executor: the kit it fetched, the staging root its
// caller materialized context into, and the local facilities (scrubber,
// credential resolver, preflighted harness) it built along the way.
type podExecutorWiring struct {
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
	HarnessInfo     harnessPreflightInfo
}

// podAgenticExecutorInput assembles the executor input for a pod stage.
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
func podAgenticExecutorInput(w podExecutorWiring) agenticExecutorInput {
	return agenticExecutorInput{
		GooberName:       w.Kit.Envelope.Goober,
		Goobers:          w.Kit.Goobers,
		Instructions:     w.Kit.Instructions,
		Assets:           w.Kit.AssetBundles(),
		HarnessInfo:      w.HarnessInfo,
		AdapterRegistry:  w.AdapterRegistry,
		EnvCapabilities:  w.Kit.EnvCapabilities,
		Resolver:         w.Resolver,
		Grants:           w.Grants,
		SharedRegistry:   w.Registry,
		RunsDir:          w.RunsDir,
		SandboxPosture:   instance.SandboxPosture(w.Kit.SandboxPosture),
		ArtifactRecorder: podArtifactRecorder{stderr: w.Stderr, scrubber: w.Scrubber, dir: w.RunsDir},
		SecretRegistrar:  w.Registry,
		AgenticAdapter:   newAgenticAdapter,
	}
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

// RecordArtifact scrubs ONCE, derives the content address of the scrubbed
// bytes, and hands those SAME bytes to recordStageArtifacts — which is both
// the journal emit and (#3823) the blob-plane write-through. Re-scrubbing
// between the digest and the PUT would let the stored content drift from the
// address it is stored under, and blobstore.Dir.Get re-verifies the digest on
// the way out, so the drift would surface as a permanently unavailable blob
// rather than as an error here.
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
// span produced in a pod lands under the same name it would locally — and,
// BECAUSE it goes through RecordArtifact, it is also published to the blob
// plane under the exact digest its ref names.
//
// THAT PUBLISH IS THE HALF THAT MAKES THE DAEMON'S SpanSource WIRING MEAN
// ANYTHING (#3805). The engine workflow never holds the transcript: it emits a
// pointer-only span op (internal/engine/journal.go JournalSpanOp) and the
// daemon's live writer fetches the bytes by digest FROM THE BLOB STORE. Until
// something PUT them there, wiring a span source alone would only change the
// recorded failure from "no span source configured" to "blobstore: blob not
// found" — the same span_unavailable code, the same missing transcript.
//
// PRECISELY: the daemon does already receive these exact bytes, at this exact
// digest, moments earlier — recordStageArtifacts puts them on the wire as a
// livejournal.OpArtifact and the daemon writes them under runs/<id>/artifacts/.
// What it could not do is FIND them by digest: the span source reads the blob
// store, and nothing mirrored an artifact op into it. (The alternative design —
// have the daemon mirror artifact ops named spans/* into its store — is
// recorded on #3805; it trades a second transfer for a dependency on artifact
// NAMING, and loses both copies if the artifact emit fails.)
//
// ONE WRITE-THROUGH, NOT TWO. #3823's putStageArtifactBlob, inside
// recordStageArtifacts, already publishes every content-addressed stream a pod
// produces — spans included, since they reach it through RecordArtifact. A
// second, span-specific PUT beside it would duplicate the transfer, carry its
// own timeout, and give the same bytes two chances to disagree about which
// slice the digest committed to. The properties #3805 needs are exactly the
// ones that helper already has: the bytes the ref was derived from, best effort
// with a stderr line, under one batch budget (blobWriteThroughBudget).
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
			Kind: livejournal.OpAppend,
			Key:  fmt.Sprintf("%s/%s/%d", os.Getenv(dispatcher.EnvStage), ev.Type, ev.Seq),
			// The daemon's replayClock adopts THIS field as the event's own
			// Time (livejournal.applyOp: run.clock.set(op.Time)) — a pod has
			// no journal-plane clock of its own to inherit one from, so an
			// unstamped Op here durably persists agent.lifecycle/agent.message
			// events at 0001-01-01T00:00:00Z (#3774), which the run-stalled
			// watchdog then either misreads as ancient activity or (post
			// #3775/#3776) declines to judge at all.
			Event: &event,
			Time:  time.Now().UTC(),
		}},
	})
	return err
}
