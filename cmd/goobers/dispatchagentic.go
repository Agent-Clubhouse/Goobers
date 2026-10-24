package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/workspacedelta"
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
func runAgenticStage(ctx context.Context, stdout, stderr io.Writer) (out stageOutcome) {
	digest := strings.TrimSpace(os.Getenv(dispatcher.EnvAgenticKitDigest))
	if digest == "" {
		// These first two refusals precede the kit, so they precede
		// kit.IsReview() and the fail closure below: the pod cannot know
		// whether it was dispatched as a review, and so cannot stamp the
		// review-only Retryable class. The ENGINE knows (input.Review) and
		// classifies both codes as infrastructure faults on its side
		// (engine.reviewActivityResult, #3888).
		return stageOutcome{Result: failureEnvelope(dispatcher.CodeAgenticKitMissing, "no agentic kit digest was stamped on this pod")}
	}
	kit, err := fetchAgenticKit(ctx, digest)
	if err != nil {
		// Fail closed and name the kit. A stage that proceeded without its kit
		// would run with no instructions at all.
		return stageOutcome{Result: failureEnvelope(dispatcher.CodeAgenticKitUnavailable, err.Error())}
	}
	ctx, usage := newSupervisedUsageContext(ctx, kit.UsageAllowance)
	ctx = invoke.WithAgentUsageReporter(ctx, usage.report)
	ctx = invoke.WithAgentUsageStream(ctx, usage.report)
	defer func() { settleSupervisedUsage(&out, usage) }()
	if usage.budgetCause() != nil {
		return stageOutcome{}
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
	fail := func(code string, err error) stageOutcome {
		envelope := failureEnvelope(code, err.Error())
		if kit.IsReview() && reviewSubstrateFailure(code) {
			envelope.Error.Retryable = substrateRetryable(err)
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
	minted, mintedScheme, err := resolveStageCredentialsWithScheme(ctx)
	if err != nil {
		return fail("credential_resolve_failed", err)
	}
	workspace, err := os.Getwd()
	if err != nil {
		return fail("workspace_provision_failed", fmt.Errorf("resolve workspace: %w", err))
	}
	// The checkout may use a credential the AGENT never receives (#3770): it
	// provisions the working tree and is excluded from buildPodAgenticExecutor
	// below, so the goober's resolver and its environment see only what the
	// stage actually declared.
	// checkoutCreds is minted plus that checkout-only credential.
	checkoutCreds, checkoutScheme, checkoutErr := podCheckoutCredentials(ctx, minted, mintedScheme)
	if checkoutErr != nil {
		return fail("credential_resolve_failed", checkoutErr)
	}
	if err := checkoutRepoWorkspace(ctx, workspace, stderr, checkoutCreds, checkoutScheme); err != nil {
		// A pull request head that moved under the run surrenders its own
		// code (#6019) — non-retryable, like every DivergedError (#5948) —
		// so the engine ends the run superseded rather than failed.
		code := workspaceProvisionFailureCode(err)
		out := fail(code, err)
		out.WorkspaceHeadMoved = code == runsemantics.WorkspaceHeadMovedErrorCode
		return out
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
		return fail("workspace_provision_failed", fmt.Errorf("create runs dir: %w", err))
	}

	// Fetch this stage's upstream artifacts BEFORE the harness looks for them.
	// See dispatchcontext.go: without this every artifact-backed pointer fails
	// to resolve, because a pod's staging root starts empty.
	if err := materializePodContext(ctx, runsDir, kit.Envelope, stderr); err != nil {
		return fail("context_materialize_failed", err)
	}

	// Generated children receive the host runner's captured reviewer diff through
	// contract-bound context. Their portable checkout deliberately has no host
	// ancestry and cannot recompute that diff. Ordinary pod reviews still compute it.
	if kit.IsReview() && isolatedChildWorkspace(ctx) && kit.ReviewRequiresDiff {
		if err := requireCapturedChildReviewDiff(kit.Envelope, os.Getenv(dispatcher.EnvStage)); err != nil {
			return fail("reviewer_diff_missing", err)
		}
	}
	if kit.IsReview() && !isolatedChildWorkspace(ctx) {
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
		pointer, derr := recordPodReviewerDiff(ctx, workspace, runsDir, os.Getenv(dispatcher.EnvStage), checkoutCreds, stderr)
		if derr != nil {
			return fail("reviewer_diff_failed", derr)
		}
		if pointer != nil {
			kit.Envelope.ContextPointers = append(kit.Envelope.ContextPointers, *pointer)
		} else if kit.ReviewRequiresDiff {
			verdict, err := missingReviewerDiff(kit.Envelope, os.Getenv(dispatcher.EnvStage))
			if err != nil {
				return fail("reviewer_diff_missing", err)
			}
			return stageOutcome{
				Result:  apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "empty-diff verdict surrendered"},
				Verdict: verdict,
			}
		}
	}

	ctx = podAgenticMergeAuthorityContext(ctx, kit.Envelope)
	exec, scrubber, err := buildPodAgenticExecutor(kit, stderr, checkoutCreds, runsDir)
	if err != nil {
		return fail("agentic_executor_unavailable", err)
	}
	executionStarted := false
	defer func() {
		out.OutboxScrubber = scrubber
		out.OutboxEligible = executionStarted
	}()

	if kit.IsReview() {
		executionStarted = true
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
			// without a verdict (no completion, a harness exit, a timeout —
			// #5543) as an invoke.InfrastructureFailure, and the self arm's
			// ReviewGoober hands that class to classifySeamError, so the
			// gate's evaluator retry bound covers it there. A pod that dropped
			// the class would fail the run where the worker would retry. A
			// verdict the schema refused, or a harness that refused the
			// session, carries no marker and stays the review's own outcome.
			outcome := fail("agentic_review_failed", err)
			outcome.Result.Error.Retryable = invoke.IsInfrastructureFailure(err)
			return outcome
		}
		return stageOutcome{
			Result:  apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Summary: "reviewer verdict surrendered"},
			Verdict: &verdict,
		}
	}

	executionStarted = true
	result, err := exec.Invoke(ctx, kit.Envelope)
	if err != nil {
		return fail("agentic_invocation_failed", err)
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

// substrateRetryable narrows reviewSubstrateFailure's Retryable stamp to what
// the failure actually is, rather than marking every substrate code
// unconditionally retryable. A *dispatcher.CredentialResolveRefusal is the
// credential plane's own judgement on THIS request (403
// capability_undeclared, 409 gate_pin_missing, and the like) — deterministic,
// so a fresh pod asks the plane the same question and gets the same refusal,
// and spending a retry on it is pointless: Retryable is false. Everything
// else this function sees — a dial error or a 5xx the plane never got to
// answer, a checkout or context-materialize fault that never asked the plane
// anything, a pod-local harness-construction fault (agentic_executor_
// unavailable's own errors never wrap a plane response at all) — is
// transport- or infra-shaped, exactly what a fresh pod's retry exists to
// ride out, so it keeps the historical Retryable=true. A
// *workspacedelta.DivergedError is deterministic too (#5948) and is not
// retried.
func substrateRetryable(err error) bool {
	var refusal *dispatcher.CredentialResolveRefusal
	if errors.As(err, &refusal) {
		return !refusal.Deterministic()
	}
	// The workspace-delta ancestry guard's refusal is deterministic in the
	// same sense (#5948): the branch carries commits the delta lacks, and a
	// fresh pod clones that same branch and fetches that same delta, so a
	// retry re-derives the identical refusal. Marking it retryable spent the
	// gate's evaluator retry bound on it and then failed the run anyway.
	var diverged *workspacedelta.DivergedError
	return !errors.As(err, &diverged)
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

type checkpointPodArtifacts struct {
	podArtifactRecorder
	livejournal.TranscriptTransport
}

func podCheckpointRecorder(w runtimewiring.PodExecutorWiring) runner.ArtifactRecorder {
	recorder := podArtifactRecorder{stderr: w.Stderr, scrubber: w.Scrubber, dir: w.RunsDir}
	endpoint := strings.TrimSpace(os.Getenv(dispatcher.EnvDaemonAPI))
	blobs := podBlobClient()
	if endpoint == "" {
		return recorder
	}
	transport := livejournal.TranscriptTransport{
		// Placement supplies the journal scope in the pod environment, just
		// as it does for artifact emission. Review kits may omit gaggle.
		RunID: os.Getenv(dispatcher.EnvRunID), Gaggle: os.Getenv(dispatcher.EnvGaggle),
		// TranscriptTransport supplies separate checkpoint/final deadlines.
		Emitter: &livejournal.HTTPEmitter{BaseURL: endpoint, Token: os.Getenv(dispatcher.EnvPodToken)},
	}
	if blobs != nil {
		transport.Blobs = blobs
	}
	return checkpointPodArtifacts{podArtifactRecorder: recorder, TranscriptTransport: transport}
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
	timing   artifactTiming
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
	recordStageArtifactsWithTiming(context.Background(), r.stderr, map[string][]byte{name: scrubbed}, r.timing)
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

// RecordArtifactWithIntegrity satisfies the integrity recorder
// internal/executor asserts on at RUN time (executor's
// integrityArtifactRecorder). CIPollExecutor's failing-CI arm requires it to
// record ci-checks.json, and the assertion is a plain type switch that
// returns "CI failure evidence integrity recorder is unavailable" when it
// misses — so without this method a pod ci-poll would fail on exactly the
// runs the evidence exists for (#3881), while a passing poll on the same pod
// succeeded. The provenance grade is not carried through the journal plane
// today (nor is it by RecordArtifactBoundedWithIntegrity above), so it is
// accepted and dropped rather than silently changing the artifact's bytes.
func (r podArtifactRecorder) RecordArtifactWithIntegrity(name string, data []byte, _ apiv1.Integrity) (journal.Ref, error) {
	return r.RecordArtifact(name, data)
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
	return r.AppendContext(context.Background(), ev)
}

// AppendContext bounds emission by the polling stage deadline.
func (r podArtifactRecorder) AppendContext(ctx context.Context, ev journal.Event) error {
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
	_, err := emitter.Emit(ctx, livejournal.EmitRequest{
		RunID:  runID,
		Gaggle: os.Getenv(dispatcher.EnvGaggle),
		Ops: []livejournal.Op{{
			Kind: livejournal.OpAppend,
			Key:  podAgentEventOpKey(ev),
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

func podAgenticExecutorInput(w runtimewiring.PodExecutorWiring) runtimewiring.AgenticExecutorInput {
	input := runtimewiring.PodAgenticExecutorInput(w)
	input.ArtifactRecorder = podCheckpointRecorder(w)
	input.ChildWorkflows = runtimewiring.DeliveredChildWorkflowAccess(w.Registry, parentPodAccess(w.Registry))
	return input
}

// buildPodAgenticExecutor also returns the scrubber chained over the
// executor's own secret registry, so the outbox export redacts every value the
// executor registered (minted credentials included).
func buildPodAgenticExecutor(kit *agentickit.Kit, stderr io.Writer, minted []dispatcher.MintedCredential, runsDir string) (invoke.Goober, journal.Scrubber, error) {
	var scrubber journal.Scrubber
	exec, err := runtimewiring.BuildPodAgenticExecutor(kit, stderr, minted, runsDir, func(input runtimewiring.AgenticExecutorInput) runtimewiring.AgenticExecutorInput {
		scrubber = journal.Chain(input.SharedRegistry, journal.NewPatternScrubber())
		return podAgenticExecutorInput(runtimewiring.PodExecutorWiring{Kit: kit, RunsDir: runsDir, Stderr: stderr, Scrubber: input.SharedRegistry, Registry: input.SharedRegistry, Resolver: input.Resolver, Grants: input.Grants, AdapterRegistry: input.AdapterRegistry, HarnessInfo: input.HarnessInfo})
	})
	return exec, scrubber, err
}
