package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// Defaults for Config fields left zero. Each is a named constant so a
// diagnostic can cite the bound that fired rather than a magic number.
const (
	// DefaultLinuxScheduleToStart bounds how long a dispatch waits for
	// capacity on a Linux runner before failing with a named diagnostic —
	// the same 15-minute class as the engine's stage schedule-to-start bound
	// (decision record D4 checkpoint 2: capacity-exhausted is a bounded,
	// named runtime failure).
	DefaultLinuxScheduleToStart = 15 * time.Minute
	// DefaultWindowsScheduleToStart is deliberately HIGHER (decision record
	// D9/D12, architecture §6): Windows scale-from-zero absorbs node
	// provisioning and multi-GB image pulls, and a Windows dispatch that
	// exceeds the Linux default must produce a cause-naming diagnostic, not
	// a generic timeout.
	DefaultWindowsScheduleToStart = 45 * time.Minute
	// DefaultDeadlineMargin pads the stage timeout into the pod's
	// activeDeadlineSeconds — the always-on orphan backstop (dispatcher §5):
	// wide enough that the stage's own policy-classed timeout enforcement
	// always fires first, bounded so a dispatcher crash cannot leak a pod
	// indefinitely.
	DefaultDeadlineMargin = 10 * time.Minute
	// DefaultStageTimeout backs a stage that declares no timeout, so every
	// pod still carries a finite activeDeadlineSeconds (the backstop is
	// always-on, never conditional on declaration).
	DefaultStageTimeout = time.Hour
	// DefaultSupervisionInterval paces the supervise loop's pod polls and
	// liveness relays.
	DefaultSupervisionInterval = 15 * time.Second

	// DefaultUnschedulableGrace is how long a pod may report Unschedulable
	// before its attempt is failed. Five minutes is chosen to outlast an AKS
	// node scale-up, which is the legitimate reason a pod is briefly
	// unschedulable; a genuinely impossible placement stays unschedulable
	// forever, so the grace costs it five minutes and nothing else.
	DefaultUnschedulableGrace = 5 * time.Minute
	// DefaultCapacityInterval paces capacity re-probes during the bounded
	// schedule-to-start wait.
	DefaultCapacityInterval = 15 * time.Second
)

// DefaultTmpfsSizeLimit is the tmp:ephemeral tmpfs sizeLimit when the
// operator configures none. It is ALWAYS stamped explicitly (dispatcher §5,
// constraint (d)): an absent sizeLimit on a memory-backed emptyDir defaults
// to half the node's RAM, which no runner ceiling ever accounted for.
var DefaultTmpfsSizeLimit = resource.MustParse("512Mi")

// Config is the dispatcher's per-instance wiring.
type Config struct {
	// Namespace is the gaggle namespace stage pods are created in.
	Namespace string
	// Owner identifies THIS dispatcher process among the workers sharing a
	// namespace. It is stamped on every pod as LabelOwner and is the scope
	// SweepOrphans sweeps within, so it must be stable across a restart of
	// the same worker and distinct between workers. The worker wires its
	// hostname — in-cluster, its pod name: stable while the pod lives, unique
	// per replica.
	//
	// A rollout gives the replacement worker a NEW pod name, so stage pods
	// left by the outgoing one fall outside every sweep's scope. That is the
	// intended trade: the sweep's job is to reclaim ITS OWN interrupted
	// attempts, and the always-on activeDeadlineSeconds stamp (dispatcher §5)
	// is what bounds every other leak. Deleting a pod on a guess is the
	// failure this whole path is built to avoid.
	//
	// Empty stamps no owner label and makes SweepOrphans refuse: an ownerless
	// fleet cannot be swept safely by one of its members.
	Owner string
	// EmbeddedCommit is this dispatcher binary's embedded commit sha
	// (internal/version.Commit at wiring) — the left side of the decision-009
	// version-skew tag comparison.
	EmbeddedCommit string
	// EmbeddedVersion is the binary's embedded release version, the
	// comparison basis for release-tagged images (DI-6's release-time
	// reading).
	EmbeddedVersion string
	// TokenMinter issues the per-run bearer a stage pod presents to the
	// daemon write API. Nil leaves PodToken empty, which is correct only when
	// the API needs no pod authentication (a loopback, null-auth daemon
	// sharing this process). In a split deployment it MUST be set, or the pod
	// surrenders unauthenticated and the daemon rejects it (Goobers#3701).
	TokenMinter TokenMinter

	// KitWriter publishes an agentic stage's execution kit and returns its
	// content address. Declared as a SEAM for the same reason as TokenMinter:
	// building a kit needs the instance configuration, which lives in the
	// worker and must not become a dependency of this package.
	//
	// Nil means agentic stages cannot be dispatched — Dispatch refuses them
	// rather than creating a pod that would find no kit and fail obscurely.
	KitWriter KitWriter

	// BlobEndpoint is the URL stage pods fetch/put artifact digests against
	// (decision 010) — stamped into every pod's environment, every runner
	// class INCLUDED restricted (without it a restricted stage hangs at
	// materialize; it is the class's own data path, not a grant to withhold).
	BlobEndpoint string
	// WriteAPIBase is the daemon write API base URL stage pods emit journal
	// events to and resolve credentials from (GOOBERS_DAEMON_API).
	WriteAPIBase string
	// EnvPassthrough is the instance's RunnerConfig.EnvPassthrough (#736): the
	// operator-declared env var NAMES carried into a stage subprocess on top of
	// procenv's built-in default-deny allowlist.
	//
	// It matters here only for a runner class enforcing env:default-deny
	// (#3725): that class's pods filter their inherited container environment
	// through the same allowlist the local executor uses, and an operator who
	// declared a passthrough name expects it to reach a stage on EITHER
	// substrate. Names only — the VALUES come from the pod's own image, not from
	// the daemon's process, which is the asymmetry stageEnvironment()'s old
	// "true parity needs the instance's envPassthrough threaded into the pod"
	// note was describing.
	//
	// Unset leaves the pod on procenv's built-in list alone, which fails closed.
	EnvPassthrough []string
	// TmpfsSizeLimit overrides DefaultTmpfsSizeLimit; zero uses the default.
	TmpfsSizeLimit resource.Quantity
	// DeadlineMargin overrides DefaultDeadlineMargin; zero uses the default.
	DeadlineMargin time.Duration
	// LinuxScheduleToStart / WindowsScheduleToStart override the capacity
	// wait bounds; zero uses the defaults.
	LinuxScheduleToStart   time.Duration
	WindowsScheduleToStart time.Duration
	// SupervisionInterval overrides DefaultSupervisionInterval; zero uses
	// the default.
	SupervisionInterval time.Duration
	// UnschedulableGrace overrides DefaultUnschedulableGrace; zero uses the
	// default.
	UnschedulableGrace time.Duration
	// CapacityInterval overrides DefaultCapacityInterval; zero uses the
	// default.
	CapacityInterval time.Duration
}

// ownerLabel is Owner rendered into label grammar. The stamp and the sweep's
// selector both go through it, so they cannot disagree about what an owner's
// pods are labeled with.
func (c Config) ownerLabel() string {
	if strings.TrimSpace(c.Owner) == "" {
		return ""
	}
	return sanitizeNameSegment(c.Owner, 63)
}

func (c Config) tmpfsSizeLimit() resource.Quantity {
	if c.TmpfsSizeLimit.IsZero() {
		return DefaultTmpfsSizeLimit.DeepCopy()
	}
	return c.TmpfsSizeLimit
}

func (c Config) deadlineMargin() time.Duration {
	if c.DeadlineMargin <= 0 {
		return DefaultDeadlineMargin
	}
	return c.DeadlineMargin
}

// unschedulableGrace is how long a pod may report Unschedulable before the
// attempt is failed. It is a GRACE, not a check interval: Unschedulable is
// routinely transient while the cluster autoscaler adds a node, so failing on
// first sight would break legitimate scale-up. It only has to outlast a node
// coming up.
func (c Config) unschedulableGrace() time.Duration {
	if c.UnschedulableGrace <= 0 {
		return DefaultUnschedulableGrace
	}
	return c.UnschedulableGrace
}

func (c Config) supervisionInterval() time.Duration {
	if c.SupervisionInterval <= 0 {
		return DefaultSupervisionInterval
	}
	return c.SupervisionInterval
}

func (c Config) capacityInterval() time.Duration {
	if c.CapacityInterval <= 0 {
		return DefaultCapacityInterval
	}
	return c.CapacityInterval
}

// scheduleToStart returns the bounded capacity wait for a runner OS (D12:
// higher on Windows).
func (c Config) scheduleToStart(os string) time.Duration {
	if os == osWindows {
		if c.WindowsScheduleToStart > 0 {
			return c.WindowsScheduleToStart
		}
		return DefaultWindowsScheduleToStart
	}
	if c.LinuxScheduleToStart > 0 {
		return c.LinuxScheduleToStart
	}
	return DefaultLinuxScheduleToStart
}

// linuxScheduleToStart is the Linux default bound, needed by the Windows
// cause-naming diagnostic ("over the Linux default bound").
func (c Config) linuxScheduleToStart() time.Duration {
	if c.LinuxScheduleToStart > 0 {
		return c.LinuxScheduleToStart
	}
	return DefaultLinuxScheduleToStart
}

// Attempt is one stage attempt to dispatch: the identity, requirement, and
// budget facts the pod spec is a pure function of.
type Attempt struct {
	// RunID, Gaggle, Workflow, and Stage identify the attempt; Number is the
	// 1-based attempt ordinal. Together they name the fresh pod — a new
	// Number is a new pod by construction (decision record D1).
	RunID    string
	Gaggle   string
	Workflow string
	Stage    string
	Number   int
	// LedgerTouching marks a stage that mutates instance-ledger state
	// (claims, close-out). Such a stage NEVER places on Windows
	// (architecture §6/§11.7) — the solver refuses it upstream and the
	// dispatcher re-asserts it with a named diagnostic rather than trusting
	// that the refusal happened.
	LedgerTouching bool
	// CPU, Memory, and Disk are the stage's declared runsOn minimums as
	// Kubernetes quantity strings ("" = none). They become pod resource
	// REQUESTS; limits come from the runner ceiling, never from the stage
	// (dsl-3.0.md D2).
	CPU    string
	Memory string
	Disk   string
	// OwningWorkflowID is the id of the Temporal workflow execution driving
	// this attempt — the caller's own execution, stamped on the pod as
	// AnnotationOwningWorkflowID and describable VERBATIM by the orphan
	// sweep.
	//
	// Distinct from two neighbours it is easy to confuse it with:
	// Config.Owner / LabelOwner names the dispatcher PROCESS that created the
	// pod (the sweep's scope), and Workflow above is the goobers workflow
	// NAME from the DSL. This is a Temporal execution id, and it is the only
	// field on the attempt that addresses one.
	//
	// Empty means the caller did not state a driver. The pod is then stamped
	// without the annotation and the sweep leaves it alone forever rather
	// than guessing an id — see stampIdentityAnnotations and podAttempt.
	OwningWorkflowID string
	// Restrictions is the stage's effective restriction requirement
	// (declared ∪ mandates, as solved). It must be a subset of the resolved
	// runner's enforced set; the dispatcher refuses the mismatch at create.
	Restrictions []string
	// RunsOnCapabilities is the stage's effective runsOn.capabilities
	// requirement (declared ∪ derived ∪ gaggle floor, as solved) — the
	// RUNNER-capability tags, not the credential Capabilities below. The
	// dispatcher reads exactly one token from it: runnercap.CapabilityWindowsAdmin,
	// which decides a Windows pod's container identity (#3619). A stage that
	// requires it on a runner that does not provide it is refused at create;
	// a stage that does not require it runs as ContainerUser even on a runner
	// that provides it.
	RunsOnCapabilities []string
	// Timeout is the stage's declared timeout; zero uses
	// DefaultStageTimeout. activeDeadlineSeconds = Timeout + margin.
	Timeout time.Duration
	// PodToken is the per-run bearer (internal/podauth) minted at dispatch,
	// delivered to the pod as GOOBERS_POD_TOKEN.
	PodToken string
	// Command, Script, and Env are the stage's DeterministicRun content
	// (apiv1.DeterministicRun.Command/Script/Env), carried from the pinned
	// workflow definition so the pod spec can stamp what the pod actually
	// executes (#3699). Never both Command and Script set — the same
	// exclusivity DeterministicRun itself enforces upstream.
	Command []string
	Script  string
	Env     map[string]string

	// Inputs is the stage's declared inputs (apiv1.InvocationEnvelope.Inputs),
	// rendered to strings. The dispatcher stamps them as GOOBERS_INPUT_<KEY>
	// exactly as the local executor does, so a stage reads its inputs the same
	// way on both substrates — and so the in-pod executor can find the
	// declared resultFile, which it must lift into Outputs.
	//
	// Without this a pod-executed stage saw EVERY declared input as unset and
	// surrendered stdout instead of its result file, which silently changed
	// gate outcomes rather than failing (Goobers#3699 v1 cut).
	Inputs map[string]string

	// CheckoutCapability is the capability the pod may mint solely to provision
	// a repo workspace (#3770) — never exported to the stage's environment.
	CheckoutCapability string
	// WorkspaceDelta is the blob digest of the git bundle carrying what earlier
	// stages of this run committed (#3763). The pod applies it after checkout so
	// this stage continues from their work instead of from base — the worker
	// gets this for free from a shared branch ref, a pod does not.
	WorkspaceDelta string
	// WorkspaceBranch is the run's rebound workspace branch (#392): empty while
	// the run is on the branch the pod can derive for itself (namespace +
	// workflow + run id), and the branch a stage bound with the well-known
	// `workspaceBranch` output once one has — pr-remediation binds it to the
	// claimed PR's head, so every later stage works on the PR's branch.
	//
	// A pod cannot derive this, which is the whole reason it is carried: the
	// derivation composes the RUN branch, which is exactly the branch a rebound
	// run is not on.
	WorkspaceBranch string
	// SyncBase asks the in-pod checkout to merge the freshly fetched base into
	// the branch it landed on, the way the local runner's worktree provisioner
	// does for a stage declaring run.syncBase (#813). Only meaningful for a
	// writable repo workspace, and only for a branch that already existed —
	// a branch created at base is already synced by construction.
	SyncBase bool
	// Capabilities is the stage's declared credential capabilities
	// (apiv1.InvocationEnvelope.Capabilities). The pod resolves them against
	// the daemon's credential plane at stage START — the dispatch payload
	// carries the capability NAMES only, never a secret (#2931/DS10).
	Capabilities []string

	// CLIStage marks a stage whose command IS the goobers CLI. Only such a
	// stage receives the run's operational identity — the same least-privilege
	// boundary the local executor draws (#322): a stage running the project's
	// own build/test suite must not see GOOBERS_* vars, or a self-hosting
	// project's tests are silently perturbed by the live run.
	CLIStage bool
	// Workspace is the workspace mode the stage declared. A repo workspace
	// makes the in-pod executor check the repository out before running the
	// command; scratch (or empty) leaves it an empty directory, which is what
	// every pod-executed stage got before pod-side checkout existed.
	Workspace string
	// Agentic marks a stage the pod executes by invoking a goober through its
	// harness rather than by running a declared command. Such a stage needs its
	// whole InvocationEnvelope and its goober's resolved execution inputs, which
	// travel as a claim check (internal/agentickit) rather than on the pod spec.
	Agentic bool
	// Envelope is the invocation an AGENTIC stage executes. Nil for every
	// deterministic stage, whose inputs are the declared command and its
	// stamped environment.
	//
	// It rides the attempt rather than the pod: the kit writer needs it to
	// resolve the goober, and the pod receives it inside the verified kit — not
	// on a pod spec, where the run's goal and ownership boundary would be
	// readable by anything with namespace read.
	Envelope *apiv1.InvocationEnvelope
	// KitDigest is the published kit's content address, set by Dispatch for an
	// agentic attempt and stamped on the pod. Never set by a caller.
	KitDigest string
	// RunContext is the operational identity a goobers-CLI stage reads to learn
	// which run it belongs to and which repository it was routed to. Empty for
	// every other stage.
	RunContext map[string]string
	// ExtraLabels and ExtraAnnotations carry any workflow/gaggle/stage
	// -supplied pod metadata. Keys in the goobers.dev/ namespace are REFUSED
	// at create (§3: the runner-class label is derived and non-overridable;
	// RBAC cannot constrain label values, so an input-influenced class label
	// is privilege escalation into a broader egress grant).
	ExtraLabels      map[string]string
	ExtraAnnotations map[string]string
}

func (a Attempt) stageTimeout() time.Duration {
	if a.Timeout <= 0 {
		return DefaultStageTimeout
	}
	return a.Timeout
}

// RunnerSpec is the dispatcher's view of one resolved runner: the inventory
// entry's claims plus its classified host kind. JSON tags are part of the
// contract: the #3588 cutover pins the eligible set into engine.RunInput at
// run start (the WF-016 snapshot), so this shape is serialized into workflow
// input and history and must stay replay-decodable.
type RunnerSpec struct {
	// Name is the runners-inventory entry name.
	Name string `json:"name"`
	// OS is the runner's claimed operating system (runnersolve enum:
	// "linux", "windows", "macOS").
	OS string `json:"os,omitempty"`
	// HostKind classifies Host (self | image | deployment).
	HostKind instance.RunnerHostKind `json:"hostKind"`
	// Host is the raw host value: "self", an image reference, or a
	// Deployment name.
	Host string `json:"host"`
	// CPU, Memory, and Disk are the runner's declared ceilings as Kubernetes
	// quantity strings ("" = no ceiling) — they become pod resource LIMITS.
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
	Disk   string `json:"disk,omitempty"`
	// Restrictions are the isolation effects this runner enforces — the
	// resolved restriction set the runner-class label derives from.
	Restrictions []string `json:"restrictions,omitempty"`
	// Capabilities is the runner's provides.capabilities claim set. The
	// dispatcher consults one token of it — runnercap.CapabilityWindowsAdmin,
	// the claim that a Windows class may run a stage as
	// ContainerAdministrator (#3619); everything else is the solver's.
	// Additive on the pinned-placement wire contract (omitempty): a recorded
	// history without it decodes to a runner claiming nothing, which is the
	// fail-closed reading (no admin identity).
	Capabilities []string `json:"capabilities,omitempty"`
}

// SpecFromEntry converts a validated inventory entry into the dispatcher's
// runner view, classifying its host kind.
func SpecFromEntry(e instance.RunnerEntry) (RunnerSpec, error) {
	kind, err := instance.ClassifyRunnerHost(e.Host)
	if err != nil {
		return RunnerSpec{}, fmt.Errorf("dispatcher: runner %q: %w", e.Name, err)
	}
	restrictions := make([]string, 0, len(e.Restrictions))
	for _, r := range e.Restrictions {
		restrictions = append(restrictions, string(r))
	}
	return RunnerSpec{
		Name:         e.Name,
		OS:           string(e.Provides.OS),
		HostKind:     kind,
		Host:         e.Host,
		CPU:          e.Provides.CPU,
		Memory:       e.Provides.Memory,
		Disk:         e.Provides.Disk,
		Restrictions: restrictions,
		Capabilities: append([]string(nil), e.Provides.Capabilities...),
	}, nil
}

// PodAPI is the narrow Kubernetes surface the dispatcher needs — exactly the
// §4 verb set (pods create/delete/get/list; apps/deployments GET only, the
// DI-9 template read). An implementation is wiring; tests use fakes.
type PodAPI interface {
	// CreatePod creates the pod in its manifest's namespace.
	CreatePod(ctx context.Context, pod *corev1.Pod) error
	// GetPod reads one pod; a NotFound must surface as an error the
	// supervise loop treats as terminal-unknown.
	GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error)
	// DeletePod deletes one pod; deleting an already-absent pod must not
	// error (disposal is idempotent).
	DeletePod(ctx context.Context, namespace, name string) error
	// ListPods lists pods matching every label in selector.
	ListPods(ctx context.Context, namespace string, selector map[string]string) ([]corev1.Pod, error)
	// GetDeployment reads a consumer-authored Deployment used as a pod
	// template by reference (DI-9).
	GetDeployment(ctx context.Context, namespace, name string) (*appsv1.Deployment, error)
}

// JournalRelay is the live-journal seam (D5/architecture §4): the dispatcher
// relays supervision liveness as runner.* facts. Relay failures are
// deliberately non-fatal to the attempt — the journal is how the run is
// WATCHED, not how it is executed.
type JournalRelay interface {
	// RelayLiveness reports one supervision observation for an attempt's
	// pod: the pod name and its current phase.
	RelayLiveness(ctx context.Context, attempt Attempt, pod string, phase corev1.PodPhase) error
}

// SurrenderGate confirms the disposal gate (architecture §3): blobstore
// write-through of artifacts and spans, journal emits through the write API,
// and the ResultEnvelope in the engine. The pod is disposed only work-loss-
// safe: pod-local state is by definition disposable ONLY once surrender is
// confirmed.
type SurrenderGate interface {
	// Confirmed reports whether the attempt's outputs are fully surrendered.
	Confirmed(ctx context.Context, attempt Attempt) (bool, error)
}

// CapacityProber answers whether the cluster can schedule a pod for a runner
// right now (resourcequotas/limitranges reads per dispatcher §4). nil skips
// the wait entirely (create-and-let-kubernetes-queue).
type CapacityProber interface {
	// Capacity reports whether a pod for runner is schedulable now.
	Capacity(ctx context.Context, runner RunnerSpec) (bool, error)
}

// Dispatcher executes stage attempts as fresh pods. Construct with New.
type Dispatcher struct {
	cfg      Config
	pods     PodAPI
	journal  JournalRelay
	gate     SurrenderGate
	capacity CapacityProber

	// now and sleep are test seams; defaults wire the real clock.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// New constructs a Dispatcher. pods and gate are required; journal may be nil
// (no liveness relay); capacity may be nil (no capacity wait).
func New(cfg Config, pods PodAPI, journal JournalRelay, gate SurrenderGate, capacity CapacityProber) (*Dispatcher, error) {
	if pods == nil {
		return nil, errors.New("dispatcher: PodAPI is required")
	}
	if gate == nil {
		return nil, errors.New("dispatcher: SurrenderGate is required")
	}
	if cfg.Namespace == "" {
		return nil, errors.New("dispatcher: Config.Namespace is required")
	}
	return &Dispatcher{
		cfg:      cfg,
		pods:     pods,
		journal:  journal,
		gate:     gate,
		capacity: capacity,
		now:      time.Now,
		sleep:    sleepCtx,
	}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// KitWriter publishes an agentic stage's execution kit to content-addressed
// storage the pod can read, returning the digest to stamp on the pod.
type KitWriter interface {
	WriteKit(ctx context.Context, attempt Attempt) (digest string, err error)
}

// TokenMinter issues per-run pod bearers. internal/podauth provides both
// implementations; the dispatcher depends on the seam, never the package, so
// a TokenReview-based minter can replace it without touching this file.
type TokenMinter interface {
	Mint(runID string, ttl time.Duration) (string, error)
}

// Report is one Dispatch outcome: the placement facts that feed the
// journal.Placement provenance event.
type Report struct {
	// Runner is the resolved runner name.
	Runner string
	// Local marks a self-host resolution: the stage belongs to the local
	// execution path, and no pod was created.
	Local bool
	// Pod is the created pod's name ("" when Local).
	Pod string
	// Image is the image the stage container was created with ("" when Local
	// or when the attempt failed before a pod was rendered). Read back off
	// the RENDERED pod rather than off RunnerSpec.Host, because the two
	// differ for a deployment-templated runner: Host is the Deployment name
	// and the image comes from its pod template. Decision 009 makes the tag
	// load-bearing (it IS the skew comparison), so the provenance has to name
	// the image that actually ran.
	Image string
	// Phase is the pod's terminal phase.
	Phase corev1.PodPhase
	// SurrenderConfirmed reports whether the disposal gate confirmed output
	// surrender before the pod was disposed.
	SurrenderConfirmed bool
	// Disposed reports whether the pod was deleted.
	Disposed bool
	// DisposeErr records a DeletePod failure encountered while disposing the
	// pod. It is a leak signal only — a dispose failure NEVER masks a settled
	// outcome (a confirmed success or a confirmed PodFailed), so Dispatch's
	// returned error still reflects the settled result and this field carries
	// the disposal failure alongside it. Disposed==false is the paired signal;
	// the leak is bounded by activeDeadlineSeconds and the restart reconcile
	// sweep (dispatcher §5).
	DisposeErr error
	// QueuedAt and PodStartedAt bound the schedule-to-start wait for
	// provenance.
	QueuedAt     time.Time
	PodStartedAt time.Time
}

// ErrSurrenderUnconfirmed reports a pod that reached a terminal phase without
// the disposal gate confirming output surrender. The pod is still disposed
// (one attempt per pod, D1 — the attempt retries on a FRESH pod, never this
// one); the caller classifies the attempt as infra.
var ErrSurrenderUnconfirmed = errors.New("dispatcher: stage outputs not surrendered before pod termination")

// ErrStageFailed reports a pod whose stage terminated in PodFailed. Surrender
// state and disposal are reported alongside in the Report.
var ErrStageFailed = errors.New("dispatcher: stage pod terminated in phase Failed")

// ErrPodUnschedulable reports a stage pod that no node can ever accept — a
// taint it does not tolerate, a nodeSelector nothing matches, a resource
// request larger than any node.
//
// It exists because this case is NOT covered by the leak bounds the rest of
// this file relies on. activeDeadlineSeconds is measured relative to the pod's
// StartTime, and a pod that is never scheduled never gets one, so the deadline
// never fires. MEASURED on a live cluster: a Windows stage pod with
// activeDeadlineSeconds=1500 sat Pending for TWENTY HOURS, its scheduler
// retrying 236 times, because its toleration key did not match the node taint.
//
// Without this the attempt burns the stage's entire activity budget and then
// fails as a TIMEOUT, which tells the operator the stage was slow when the
// truth is that no node could ever have run it.
var ErrPodUnschedulable = errors.New("dispatcher: stage pod cannot be scheduled onto any node")

// Dispatch executes one stage attempt on the eligible runner set (the
// solver's ELIGIBLE-SET output for this stage, in inventory order): resolve
// the runner (Linux-preferring), verify the image skew contract, wait
// bounded for capacity, create ONE fresh pod, supervise it, confirm output
// surrender, and dispose the pod. The pod is disposed on every path that
// created it — surrender-unconfirmed and stage-failed attempts return their
// typed error WITH the pod already deleted, because a retried attempt gets a
// fresh pod, never a reused one (D1).
func (d *Dispatcher) Dispatch(ctx context.Context, attempt Attempt, eligible []RunnerSpec) (Report, error) {
	runner, err := SelectRunner(attempt, eligible)
	if err != nil {
		return Report{}, err
	}
	report := Report{Runner: runner.Name, QueuedAt: d.now().UTC()}

	if runner.HostKind == instance.RunnerHostSelf {
		// host: self — the local execution path (fresh worktree per attempt,
		// createStageWorkspace semantics). No pod, and none of the pod-plane
		// contract applies (architecture §3).
		report.Local = true
		return report, nil
	}

	// Mint the pod's bearer before rendering: podspec stamps
	// GOOBERS_POD_TOKEN only when it is non-empty, so a mint failure must
	// stop the dispatch rather than silently produce a pod that cannot
	// surrender — the failure would otherwise surface much later, as an
	// unauthenticated PUT, and read as a daemon problem.
	// An agentic stage's kit is published BEFORE the pod exists, and a failure
	// to publish refuses the attempt rather than creating a pod that would find
	// no kit and fail with something obscure inside the container.
	if attempt.Agentic {
		if d.cfg.KitWriter == nil {
			return Report{}, fmt.Errorf("dispatcher: agentic stage %s of run %s requires a kit writer; none is configured", attempt.Stage, attempt.RunID)
		}
		digest, kerr := d.cfg.KitWriter.WriteKit(ctx, attempt)
		if kerr != nil {
			return Report{}, fmt.Errorf("dispatcher: publish agentic kit for run %s stage %s attempt %d: %w", attempt.RunID, attempt.Stage, attempt.Number, kerr)
		}
		if digest == "" {
			return Report{}, fmt.Errorf("dispatcher: agentic kit writer returned no digest for run %s stage %s", attempt.RunID, attempt.Stage)
		}
		attempt.KitDigest = digest
	}
	if d.cfg.TokenMinter != nil && attempt.PodToken == "" {
		token, terr := d.cfg.TokenMinter.Mint(attempt.RunID, 0)
		if terr != nil {
			return Report{}, fmt.Errorf("dispatcher: mint pod token for run %s stage %s attempt %d: %w", attempt.RunID, attempt.Stage, attempt.Number, terr)
		}
		attempt.PodToken = token
	}

	if err := d.waitForCapacity(ctx, runner); err != nil {
		return report, err
	}

	pod, err := d.renderFor(ctx, attempt, runner)
	if err != nil {
		return report, err
	}
	// Stamped from the rendered spec BEFORE the create call, so an IN-PROCESS
	// caller that inspects the returned report after a create failure still
	// sees which image was about to run.
	//
	// Scope note, because the comment used to over-promise: this does NOT
	// reach the engine's callers. engine.DispatchStage discards the report on
	// every error that left surrender unconfirmed, so the only reports whose
	// Image crosses that activity boundary are settled ones, which by
	// definition already created their pod (see engine.StagePlacement). The
	// stamp stays here regardless: it costs nothing, it is the honest ordering
	// for a direct caller, and it is what a future failure-carrying seam would
	// read.
	report.Image = stageContainerImage(pod)

	if err := d.pods.CreatePod(ctx, pod); err != nil {
		return report, fmt.Errorf("dispatcher: create pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	report.Pod = pod.Name
	report.PodStartedAt = d.now().UTC()

	phase, superviseErr := d.supervise(ctx, attempt, pod.Namespace, pod.Name)
	report.Phase = phase

	if superviseErr == nil {
		confirmed, gateErr := d.gate.Confirmed(ctx, attempt)
		report.SurrenderConfirmed = confirmed && gateErr == nil
		if gateErr != nil {
			superviseErr = fmt.Errorf("dispatcher: confirm surrender for run %s stage %s attempt %d: %w",
				attempt.RunID, attempt.Stage, attempt.Number, gateErr)
		} else if !confirmed {
			superviseErr = fmt.Errorf("%w (run %s stage %s attempt %d, pod %s)",
				ErrSurrenderUnconfirmed, attempt.RunID, attempt.Stage, attempt.Number, pod.Name)
		}
	}

	// Dispose unconditionally: one attempt per pod (D1). A dispose failure
	// must NOT mask a settled outcome. By the time we reach here superviseErr
	// == nil means the outcome is settled: supervision reached a terminal
	// phase (PodSucceeded or PodFailed) AND the gate confirmed surrender — the
	// pod's output is authoritative and downstream must read it. A dispose
	// failure in that state would otherwise turn a confirmed success or a
	// confirmed stage-failure into an infra error, discarding the surrendered
	// result and spending an infra retry re-dispatching an already-settled
	// (possibly MUTATING) stage. So record the disposal failure on the report
	// as the leak signal (report.Disposed stays false; the leak is bounded by
	// activeDeadlineSeconds and the restart reconcile sweep, dispatcher §5) and
	// let the settled path fall through: PodFailed → ErrStageFailed, success →
	// nil. When superviseErr is already non-nil there is no settled outcome to
	// protect; that infra error is returned unchanged and DisposeErr rides
	// alongside on the report. superviseErr is never overwritten here, because
	// there is no superviseErr == nil state that is not a settled outcome.
	if delErr := d.pods.DeletePod(ctx, pod.Namespace, pod.Name); delErr != nil {
		report.DisposeErr = fmt.Errorf("dispatcher: dispose pod %s/%s: %w", pod.Namespace, pod.Name, delErr)
	} else {
		report.Disposed = true
	}

	if superviseErr != nil {
		return report, superviseErr
	}
	if phase == corev1.PodFailed {
		return report, fmt.Errorf("%w (run %s stage %s attempt %d, pod %s)",
			ErrStageFailed, attempt.RunID, attempt.Stage, attempt.Number, pod.Name)
	}
	return report, nil
}

// stageContainerImage reads the stage container's image off a rendered pod.
func stageContainerImage(pod *corev1.Pod) string {
	if pod == nil {
		return ""
	}
	if stage := stageContainerIn(pod.Spec.Containers); stage != nil {
		return stage.Image
	}
	return ""
}

// renderFor renders the fresh pod for the resolved runner's host kind:
// image → dispatcher-rendered spec; deployment → instantiated from the named
// Deployment's template (DI-9). Both paths run the decision-009 skew check
// against the stage container image before anything is created.
func (d *Dispatcher) renderFor(ctx context.Context, attempt Attempt, runner RunnerSpec) (*corev1.Pod, error) {
	switch runner.HostKind {
	case instance.RunnerHostImage:
		if err := VerifySkew(d.cfg.EmbeddedCommit, d.cfg.EmbeddedVersion, runner.Host); err != nil {
			return nil, err
		}
		return RenderPod(d.cfg, attempt, runner)
	case instance.RunnerHostDeployment:
		deployment, err := d.pods.GetDeployment(ctx, d.cfg.Namespace, runner.Host)
		if err != nil {
			return nil, fmt.Errorf("dispatcher: read template deployment %q for runner %q: %w", runner.Host, runner.Name, err)
		}
		if image := templateStageImage(deployment); image != "" {
			if err := VerifySkew(d.cfg.EmbeddedCommit, d.cfg.EmbeddedVersion, image); err != nil {
				return nil, err
			}
		}
		return RenderFromTemplate(d.cfg, attempt, runner, deployment)
	default:
		return nil, fmt.Errorf("dispatcher: runner %q host kind %q cannot be rendered as a pod", runner.Name, runner.HostKind)
	}
}

// supervise polls the pod until it reaches a terminal phase, relaying each
// observation to the live journal. Errors from the relay are swallowed by
// design (the journal is observability, not control flow); errors from the
// pod read are fatal to supervision.
func (d *Dispatcher) supervise(ctx context.Context, attempt Attempt, namespace, name string) (corev1.PodPhase, error) {
	var unschedulableSince time.Time
	for {
		pod, err := d.pods.GetPod(ctx, namespace, name)
		if err != nil {
			return "", fmt.Errorf("dispatcher: supervise pod %s/%s: %w", namespace, name, err)
		}
		phase := pod.Status.Phase
		if d.journal != nil {
			_ = d.journal.RelayLiveness(ctx, attempt, name, phase)
		}
		if phase == corev1.PodSucceeded || phase == corev1.PodFailed {
			return phase, nil
		}
		// A pod no node can accept reaches NEITHER terminal phase, so without
		// this the loop polls until the activity's own deadline expires — the
		// stage's whole budget spent, and reported as a timeout rather than as
		// the placement failure it is.
		if reason, message, unschedulable := podUnschedulable(pod); unschedulable {
			if unschedulableSince.IsZero() {
				unschedulableSince = d.now()
			}
			// Grace, not a check interval: the autoscaler routinely leaves a
			// pod Unschedulable for a minute or two while a node comes up, and
			// failing on first sight would break that. An impossible placement
			// never recovers, so it costs exactly this grace.
			if waited := d.now().Sub(unschedulableSince); waited >= d.cfg.unschedulableGrace() {
				return phase, fmt.Errorf("%w (run %s stage %s attempt %d, pod %s/%s, unschedulable for %s, reason %s): %s",
					ErrPodUnschedulable, attempt.RunID, attempt.Stage, attempt.Number,
					namespace, name, waited.Round(time.Second), reason, message)
			}
		} else {
			// Recovered — a node arrived. Reset so a later, unrelated spell of
			// unschedulability gets its own full grace rather than inheriting
			// elapsed time from this one.
			unschedulableSince = time.Time{}
		}
		if err := d.sleep(ctx, d.cfg.supervisionInterval()); err != nil {
			return phase, fmt.Errorf("dispatcher: supervision of pod %s/%s interrupted: %w", namespace, name, err)
		}
	}
}

// podUnschedulable reports whether the scheduler has declared that no node can
// currently take this pod, returning the reason and message it gave.
//
// Keyed on the PodScheduled condition rather than on the phase: Pending covers
// both "waiting for a node" and "pulling an image", and only the condition
// distinguishes them. The message is carried out so the failure names the
// actual constraint — an untolerated taint reads very differently from a
// resource request no node can satisfy.
func podUnschedulable(pod *corev1.Pod) (reason, message string, unschedulable bool) {
	if pod == nil {
		return "", "", false
	}
	for i := range pod.Status.Conditions {
		c := pod.Status.Conditions[i]
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return c.Reason, c.Message, true
		}
	}
	return "", "", false
}
