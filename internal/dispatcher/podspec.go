package dispatcher

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/goobers/goobers/internal/runnercap"
)

// Dispatcher-owned pod metadata beyond the shared runnercap label contract.
// LabelRun/LabelStage/LabelAttempt are the run/attempt identity the restart
// reconcile sweep keys on (dispatcher §5: label + sweep, NOT a cross-namespace
// ownerReference).
const (
	// LabelManagedBy marks every pod this dispatcher creates; the orphan
	// sweep lists by it.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedByValue is the LabelManagedBy value.
	ManagedByValue = "goobers-dispatcher"
	// LabelRun carries the attempt's run ID (sanitized to label grammar).
	LabelRun = "goobers.dev/run"
	// LabelStage carries the stage name (sanitized to label grammar).
	LabelStage = "goobers.dev/stage"
	// LabelAttempt carries the attempt ordinal.
	LabelAttempt = "goobers.dev/attempt"
	// LabelOwner names the dispatcher process that created the pod
	// (Config.Owner, sanitized to label grammar). Decision 003 wires
	// SweepOrphans on the WORKER, and a cluster legitimately runs more than
	// one: without this the sweep's selector matches a sibling worker's live
	// stage pods too, and one worker's restart would dispose another's
	// in-flight attempts. Scoped by owner, a sweep can only ever reach pods
	// it created.
	LabelOwner = "goobers.dev/owner"
)

// Dispatcher-owned annotations carrying the attempt identity VERBATIM.
//
// The matching labels exist for humans and for selectors, so they are run
// through sanitizeNameSegment — which lowercases and maps every non-alphanumeric
// rune to '-'. That makes them unusable as an ADDRESS: the orphan sweep has to
// ask the engine whether <runID>/<stage>/<attempt> is still executing, and
// "run-2026-08-22-0001" does not survive the round trip. These carry the exact
// strings the attempt was dispatched under. (The attempt ordinal needs no
// annotation — LabelAttempt is already exact, being a decimal integer.)
const (
	// AnnotationRunID is the attempt's run ID, unsanitized.
	AnnotationRunID = "goobers.dev/run-id"
	// AnnotationStage is the attempt's stage name, unsanitized.
	AnnotationStage = "goobers.dev/stage-name"
	// AnnotationOwningWorkflowID is the Temporal workflow execution that owns
	// the dispatch: the execution whose activity created this pod, and the
	// only one whose liveness answers "is anything still driving this
	// attempt?".
	//
	// It is stamped rather than DERIVED because the attempt's own identity
	// cannot address its driver. A SCHEDULED run executes under
	// claimID+"-run" while its RunID has been rewritten to a sha256 prefix of
	// claimID (engine's RunScheduled; engine/liveness.go states the mapping),
	// so an id composed from AnnotationRunID names no execution at all — and a
	// sweep that reads "no such workflow" as "settled" would delete the pod of
	// a live, possibly mutating, stage. Composing is a lossy address on a
	// DELETE path; this is the verbatim one.
	//
	// Absent = unaddressable, not disposable: podAttempt refuses the pod and
	// the sweep leaves it to activeDeadlineSeconds.
	AnnotationOwningWorkflowID = "goobers.dev/owning-workflow-id"
)

// The pod environment contract: what a stage pod needs to reach the planes
// this substrate wires (decision 010 blob plane; D5 write API; podauth).
// GOOBERS_DAEMON_API, GOOBERS_POD_TOKEN, and GOOBERS_RUN_ID reuse the
// existing worker/stage spellings — one contract, not two.
const (
	// EnvRunID / EnvGaggle / EnvWorkflow / EnvStage / EnvAttempt identify the
	// attempt inside the pod.
	EnvRunID    = "GOOBERS_RUN_ID"
	EnvGaggle   = "GOOBERS_GAGGLE"
	EnvWorkflow = "GOOBERS_WORKFLOW"
	EnvStage    = "GOOBERS_STAGE"
	EnvAttempt  = "GOOBERS_ATTEMPT"
	// EnvBlobEndpoint is the network blob endpoint URL the pod fetches/puts
	// artifact digests against (decision 010) — present on EVERY runner
	// class, restricted included (§2a: it is the class's own data path).
	EnvBlobEndpoint = "GOOBERS_BLOB_ENDPOINT"
	// EnvDaemonAPI is the daemon write API base URL (journal emits,
	// credential resolve) — the existing worker spelling.
	EnvDaemonAPI = "GOOBERS_DAEMON_API"
	// EnvPodToken carries the per-run podauth bearer.
	EnvPodToken = "GOOBERS_POD_TOKEN"
	// EnvStageCommand carries the stage's DeterministicRun.Command as a JSON
	// array, when set — the in-pod dispatch-exec entrypoint's argv (#3699).
	EnvStageCommand = "GOOBERS_STAGE_COMMAND"
	// EnvStageScript carries the stage's DeterministicRun.Script verbatim,
	// when set.
	EnvStageScript = "GOOBERS_STAGE_SCRIPT"
	// EnvStageTimeout carries the stage's effective timeout (Go duration
	// string) so dispatch-exec bounds the command the same way the local
	// executor bounds it, independent of the pod's activeDeadlineSeconds
	// backstop (which exists to reclaim an orphaned pod, not to time the
	// stage itself).
	EnvStageTimeout = "GOOBERS_STAGE_TIMEOUT"
	// EnvStageCapabilities carries the stage's declared credential capability
	// NAMES as a JSON array. Names only: the pod resolves them against the
	// credential plane itself, so no secret ever rides a pod spec — which is
	// readable by anyone with get-pod in the namespace.
	EnvStageCapabilities = "GOOBERS_STAGE_CAPABILITIES"
	// EnvStageIsCLI marks a stage whose command is the goobers CLI, so the pod
	// keeps its run context instead of stripping it with the control plane.
	EnvStageIsCLI = "GOOBERS_STAGE_IS_CLI"

	// EnvWorkspaceDelta carries the blob digest of the git bundle holding what
	// earlier stages of this run committed (#3763). Privileged: it names a blob
	// the pod's own token can fetch, and a stage has no business reading or
	// forging it.
	EnvWorkspaceDelta = "GOOBERS_WORKSPACE_DELTA"
	// EnvCheckoutCapability names the capability the pod may mint SOLELY to
	// provision a repo workspace (#3770). Privileged, and deliberately separate
	// from EnvStageCapabilities: the resulting credential authenticates the
	// checkout inside the goobers process and is never exported to the stage's
	// environment, so a stage does not gain repository authority by needing a
	// working tree.
	//
	// Without this the checkout authenticated with the stage's BUSINESS
	// capabilities, so a repo workspace could only be provisioned when the
	// stage happened to declare a repo-shaped one for unrelated reasons —
	// open-pr declares provider:pr:write alone and could not run in a pod at
	// all. The worker has never had this problem: it provisions worktrees with
	// instance credentials regardless of what the stage declares.
	EnvCheckoutCapability = "GOOBERS_CHECKOUT_CAPABILITY"
	// EnvStageWorkspace carries the declared workspace mode so the in-pod
	// executor knows whether to provision a checkout. Privileged: a stage that
	// could rewrite it would change what the platform provisioned for it.
	EnvStageWorkspace = "GOOBERS_STAGE_WORKSPACE"

	// EnvAgenticKitDigest is the content address of the stage's execution kit.
	// Privileged: a stage that could rewrite it would choose which instructions
	// it runs under.
	EnvAgenticKitDigest = "GOOBERS_AGENTIC_KIT"

	// EnvStageEnvDefaultDeny is stamped "true" when the RESOLVED RUNNER CLASS
	// enforces env:default-deny (#3725). It is what makes that restriction real
	// in the pod: without it __dispatch-exec hands the stage the container's
	// whole os.Environ() minus the control plane, so a class declaring
	// env:default-deny got the label, the NetworkPolicy, and none of the
	// environment isolation the name promises.
	//
	// Privileged, and this is the security property, not an accident of
	// grouping: the restriction must be DISPATCHER-STAMPED and stage-invisible.
	// A stage that could read it learns which posture it is under; a stage that
	// could set or clear it turns its own isolation off — self-authorization by
	// exactly the shape EnvStageWorkspace/EnvStageIsCLI are privileged to avoid.
	// The signal never comes from the stage's own command.
	//
	// Derived from the RUNNER's enforced set rather than the stage's required
	// set, the same source every other restriction binding in this file reads
	// (tmp:ephemeral's tmpfs, fs:readonly's readOnlyRootFilesystem). A stage
	// that required nothing but LANDED on a restricted class gets the class's
	// posture — which is the case #3725 was filed about: cli-stage-probe placed
	// on linux-shell-strict and "worked because the restriction is
	// unimplemented".
	EnvStageEnvDefaultDeny = "GOOBERS_STAGE_ENV_DEFAULT_DENY"
	// EnvStageEnvAllow carries, as a JSON array, the env var NAMES the
	// dispatcher stamped ON THE STAGE'S BEHALF — its declared `env:` keys, its
	// GOOBERS_INPUT_* inputs, its run context — plus the instance's declared
	// envPassthrough. Stamped only alongside EnvStageEnvDefaultDeny, because
	// only the filter reads it.
	//
	// It exists because in a pod those variables arrive as ORDINARY container
	// environment variables, indistinguishable at os.Environ() from whatever the
	// image happens to export. Filtering the inherited environment through
	// procenv's allowlist alone would therefore drop the stage's own declared
	// env and its inputs — the same restriction-conditional, diagnosed-at-the-
	// far-side failure #3725 was filed about, one seam over. The dispatcher is
	// the only party that knows which ambient names it put there, so it says so.
	//
	// Privileged: a stage that could extend this list would re-admit exactly the
	// ambient variables the restriction exists to deny it.
	EnvStageEnvAllow = "GOOBERS_STAGE_ENV_ALLOW"
)

// DispatcherControlEnv is the set of variables the DISPATCHER stamps for its
// own in-pod runtime. They are the pod's control plane, not the stage's
// environment, and __dispatch-exec strips every one of them before handing an
// environment to the stage.
//
// EnvPodToken is the sharp one: it authorizes surrendering results for this
// run. A stage that can read it can author its own outcome — report success
// for work that failed. MEASURED before this list existed: a stage command on
// a runner declaring env:default-deny saw POD_TOKEN=PRESENT in a 24-variable
// inherited environment.
var DispatcherControlEnv = append(append([]string{}, DispatcherPrivilegedEnv...), DispatcherRunIdentityEnv...)

// DispatcherPrivilegedEnv is the half of the control plane that NO stage may
// ever see, goobers-CLI stage included. EnvPodToken is the reason the whole
// filter exists: it authorizes surrendering this run's results, so a stage that
// can read it can author its own outcome. The endpoints are here because a
// stage holding them plus any token is a step from the same place, and the
// stage-spec vars because a stage rewriting its own command/capabilities is
// self-authorization by another name.
var DispatcherPrivilegedEnv = []string{
	EnvBlobEndpoint, EnvDaemonAPI, EnvPodToken,
	EnvStageCommand, EnvStageScript, EnvStageTimeout, EnvStageCapabilities, EnvStageIsCLI,
	EnvStageWorkspace, EnvAgenticKitDigest, EnvWorkspaceDelta, EnvCheckoutCapability,
	EnvStageEnvDefaultDeny, EnvStageEnvAllow,
}

// DispatcherRunIdentityEnv is the half that is operational identity rather than
// authority: WHICH run this is, not permission to do anything as it. Knowing a
// run ID grants nothing — every plane demands the bearer token above.
//
// A goobers-CLI stage KEEPS these, because they are exactly what it needs:
// providers.BranchName composes the run branch from workflow + run, so a CLI
// stage stripped of them cannot name the branch it is supposed to push. Every
// other stage is still stripped, and that is the point of splitting rather than
// exempting — a stage running the project's own `make ci` must not see them, or
// a self-hosting project's tests are perturbed by the live run.
var DispatcherRunIdentityEnv = append([]string{
	EnvRunID, EnvGaggle, EnvWorkflow, EnvStage, EnvAttempt,
}, runContextEnv...)

// runContextEnv are the run-identity variables the DISPATCHER stamps from the
// envelope rather than deriving: which repository this run was routed to, and
// the branch conventions its run branch is composed from.
//
// They are run identity, not authority, so they live in the same half as the
// run ID: a goobers-CLI stage keeps them (providerRepo reads them), every other
// stage is stripped of them. That matters now that a repo-workspace stage also
// needs them stamped — the in-pod executor reads them to CHECK OUT the
// workspace and then strips them, so a stage running the project's own build
// still cannot see the live run (#322). Without listing them here they would
// have leaked to exactly those stages the moment checkout began stamping them.
var runContextEnv = []string{
	executorRepoProviderEnv, executorRepoOwnerEnv, executorRepoProjectEnv,
	executorRepoNameEnv, executorBranchNamespaceEnv, executorBaseBranchEnv,
	executorTriggerRefEnv,
}

// The executor package owns these names; they are restated rather than imported
// because internal/dispatcher sits beneath internal/executor and importing it
// would invert the dependency. Pinned against the originals by
// TestRunContextEnvMatchesExecutor so the restatement cannot drift.
const (
	executorRepoProviderEnv    = "GOOBERS_REPO_PROVIDER"
	executorRepoOwnerEnv       = "GOOBERS_REPO_OWNER"
	executorRepoProjectEnv     = "GOOBERS_REPO_PROJECT"
	executorRepoNameEnv        = "GOOBERS_REPO_NAME"
	executorBranchNamespaceEnv = "GOOBERS_BRANCH_NAMESPACE"
	executorBaseBranchEnv      = "GOOBERS_BASE_BRANCH"
	executorTriggerRefEnv      = "GOOBERS_TRIGGER_REF"
)

// Workspace and temp paths — the base-image contract half of the mount
// bindings (decisions 006/007).
const (
	// LinuxWorkspacePath / WindowsWorkspacePath is where the writable
	// workspace emptyDir mounts.
	LinuxWorkspacePath   = "/workspace"
	WindowsWorkspacePath = `C:\workspace`
	// LinuxHomePath is the writable HOME mount a Linux
	// fs:readonly-except-workspace pod gets alongside the workspace
	// (dispatcher §5: readOnlyRootFilesystem + writable workspace + writable
	// HOME).
	LinuxHomePath = "/home/goobers"
	// WindowsHomePath is the container user's profile in the Windows base
	// image (USER ContainerUser, DI-4): git and harness configuration land
	// here, and the tmp:ephemeral temp is nested inside it. Named so the
	// #3480 antivirus-exclusion enumeration reads it from the same contract
	// the mounts use rather than retyping it.
	WindowsHomePath = `C:\Users\ContainerUser`
	// LinuxTmpPath / WindowsTmpPath is the platform temp path the
	// tmp:ephemeral volume mounts at (Linux /tmp; Windows the profile-nested
	// temp — decision 006).
	LinuxTmpPath   = "/tmp"
	WindowsTmpPath = WindowsHomePath + `\AppData\Local\Temp`
)

// Node scheduling contract.
const (
	// NodeSelectorOSKey is the well-known node OS label.
	NodeSelectorOSKey = "kubernetes.io/os"
	// WindowsTolerationKey is the sig-windows convention for the Windows node
	// taint (node.kubernetes.io/os=windows:NoSchedule).
	WindowsTolerationKey = "node.kubernetes.io/os"
	// WindowsTolerationKeyLegacy is the key AKS actually applies to a Windows
	// node pool: kubernetes.io/os=windows:NoSchedule — the same key as the
	// well-known OS LABEL, which is why NodeSelectorOSKey above matches and the
	// toleration did not.
	//
	// MEASURED on aks-goobernetes-prod: the node carries
	// `kubernetes.io/os=windows:NoSchedule`, so a stage pod tolerating only the
	// sig-windows key sat Pending until its deadline —
	//   0/3 nodes are available: 1 node(s) had untolerated taint(s)
	// while the node selector had correctly targeted that very node. Both keys
	// are stamped: tolerating a taint a cluster does not apply costs nothing,
	// and picking one convention silently breaks every cluster using the other.
	WindowsTolerationKeyLegacy = "kubernetes.io/os"
	// WindowsRunAsUserName is the non-admin Windows container identity EVERY
	// Windows stage pod runs as unless the stage requires — and its runner
	// class provides — runnercap.CapabilityWindowsAdmin (#3619). Stamped
	// explicitly rather than inherited from the image's USER directive, so
	// least privilege is a property of the rendered spec, not of whichever
	// image a class happens to name.
	WindowsRunAsUserName = "ContainerUser"
	// WindowsAdminRunAsUserName is the administrator identity a Windows
	// stage pod runs as when — and only when — the stage requires
	// runnercap.CapabilityWindowsAdmin and the resolved runner class provides
	// it. Provided-but-not-required stays ContainerUser; required-but-not-
	// provided is refused at create (WindowsIdentityError), never defaulted.
	WindowsAdminRunAsUserName = "ContainerAdministrator"
	// StageContainerName names the stage container in dispatcher-rendered
	// pods.
	StageContainerName = "stage"
)

// DispatchExecCommand is the hidden goobers CLI entrypoint every
// dispatcher-rendered pod's Command/Args invokes to actually run the
// authored stage and surrender its result (#3699). It is a `__`-prefixed
// name (cmd/goobers's established hidden-entrypoint convention, the same
// shape as the detached run worker) — never DSL-authorable, never shown in
// --help or generated docs. Defined here rather than in cmd/goobers so the
// pod-spec side (this package) and the CLI-registration side agree on the
// literal by construction instead of by matching string literals in two
// files.
const DispatchExecCommand = "__dispatch-exec"

// LabelOverrideError is the §3 refuse-to-create: a workflow, gaggle, or stage
// attempted to influence dispatcher-owned pod metadata (the
// goobers.dev/runner-class label above all — RBAC cannot constrain label
// values, so an input-influenced class label is privilege escalation into a
// broader egress grant).
type LabelOverrideError struct {
	// Key is the refused metadata key.
	Key string
	// Source names where the attempt came from (workflow input, template).
	Source string
}

func (e *LabelOverrideError) Error() string {
	return fmt.Sprintf(
		"dispatcher: refusing to create stage pod: %s attempts to set %q — the goobers.dev/* pod metadata namespace "+
			"(the runner-class label above all) is derived by the dispatcher and non-overridable by workflow, gaggle, or stage input "+
			"(goobernetes-dispatcher.md §3, delivery decision 004)",
		e.Source, e.Key)
}

// RestrictionMismatchError is the create-time re-assertion that the stage's
// effective restriction requirement is enforced by the resolved runner —
// "asserted at dispatch, refuse-to-create on mismatch"
// (goobernetes-restrictions.md §6).
type RestrictionMismatchError struct {
	// Runner is the resolved runner.
	Runner string
	// Missing are the required effects the runner does not enforce.
	Missing []string
}

func (e *RestrictionMismatchError) Error() string {
	return fmt.Sprintf(
		"dispatcher: refusing to create stage pod on runner %q: the stage's effective restriction requirement includes [%s], which the runner does not enforce — the solver should never have placed it here",
		e.Runner, strings.Join(e.Missing, ", "))
}

// WindowsIdentityError is the create-time refusal of the Windows identity
// and binding rules (#3619; restrictions doc D4 as corrected there):
//
//   - the stage requires runnercap.CapabilityWindowsAdmin and the resolved
//     runner does not provide it (or is not Windows at all) — the solver
//     should never have placed it here, and a stage that needs admin is
//     never served as ContainerUser to fail later with Access Denied, nor
//     served as ContainerAdministrator on a class that never claimed it;
//   - the resolved Windows runner's class carries a restriction Windows
//     cannot bind (runnercap.DeclarableOnWindows is false) — the pod would
//     carry the class label and the restrictions annotation and none of the
//     isolation they name, the fail-open shape decision 007 refused for
//     readOnlyRootFilesystem.
//
// Both are re-assertions of facts the validator and the inventory loader
// already refuse; this is the dispatch-time arm ("asserted at dispatch,
// refuse-to-create on mismatch", restrictions doc §6).
type WindowsIdentityError struct {
	// Runner is the resolved runner.
	Runner string
	// Reason is the named, human-readable cause.
	Reason string
}

func (e *WindowsIdentityError) Error() string {
	return fmt.Sprintf("dispatcher: refusing to create stage pod on runner %q: %s", e.Runner, e.Reason)
}

// PodName derives the fresh pod's name for one attempt: deterministic per
// (run, stage, attempt) and unique across attempts, so "a new attempt is a
// new pod" holds by construction and a redelivered create of the SAME attempt
// collides with its own pod instead of silently making a second one.
func PodName(attempt Attempt) string {
	stage := sanitizeNameSegment(attempt.Stage, 20)
	run := sanitizeNameSegment(attempt.RunID, 24)
	return fmt.Sprintf("gbn-%s-%s-a%d", stage, run, attempt.Number)
}

// sanitizeNameSegment lowercases s, maps every character outside the DNS
// label alphabet to '-', collapses the result to at most maxLen characters,
// and trims leading/trailing hyphens. Empty input becomes "x".
func sanitizeNameSegment(s string, maxLen int) string {
	lower := strings.ToLower(s)
	var b strings.Builder
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > maxLen {
		out = out[:maxLen]
	}
	out = strings.Trim(out, "-")
	if out == "" {
		return "x"
	}
	return out
}

// RenderPod renders the fresh stage pod for an image-hosted runner
// (dispatcher §2 item 3, §5): the dispatcher-rendered spec with resource
// requests from the stage's runsOn minimums, limits from the runner ceiling,
// the restriction bindings for the runner's OS, the derived non-overridable
// runner-class label, the deny-first posture labels, the OS node selector and
// Windows toleration, and the always-on activeDeadlineSeconds backstop.
func RenderPod(cfg Config, attempt Attempt, runner RunnerSpec) (*corev1.Pod, error) {
	if err := refuseOverrides(attempt); err != nil {
		return nil, err
	}
	if err := assertRestrictionsEnforced(attempt, runner); err != nil {
		return nil, err
	}
	admin, err := assertWindowsIdentity(attempt, runner)
	if err != nil {
		return nil, err
	}

	windows := runner.OS == osWindows
	class := restrictionSet(runner.Restrictions)

	container := corev1.Container{
		Name:  StageContainerName,
		Image: runner.Host,
		// ALWAYS, not the IfNotPresent default, and this is a correctness
		// requirement rather than a freshness preference.
		//
		// Decision 009 makes the tag load-bearing: the skew check compares the
		// TAG STRING to the dispatcher's embedded commit — "no registry read,
		// the tag IS the comparison". That inference only holds if a tag maps to
		// ONE image. A registry tag is mutable, so with IfNotPresent a node that
		// cached an earlier push serves THAT content under the same tag, and the
		// skew check passes while the pod runs a different binary. The check
		// would be proving something true about the tag and false about the pod.
		//
		// MEASURED, exactly this: a Windows runner image was rebuilt at the same
		// commit to add the daemon's CA. The tag did not change, so every node
		// with the old layers kept serving them — the stage failed x509 twice
		// more, identically, after the fix was already in the registry. Verified
		// by reading imagePullPolicy off the live pod (IfNotPresent) and then
		// watching the rebuilt image be ignored.
		//
		// The cost is one manifest check per pod. Stage pods are single-use by
		// construction (D1: one attempt per pod, disposed after), so there is no
		// long-lived pod for which a cached layer would amortise anyway, and the
		// layers themselves still cache — only the manifest is re-read.
		ImagePullPolicy: corev1.PullAlways,
		Command:         []string{"goobers"},
		Args:            []string{DispatchExecCommand},
		// nil: the image path builds a fresh container, so the only names on
		// it are the ones stageEnv stamps.
		Env: stageEnv(cfg, attempt, class, nil),
	}

	// Extra (non-goobers.dev) metadata merges FIRST; the dispatcher-owned
	// stamps land last and therefore always win — a workflow must not be able
	// to overwrite even the managed-by marker (that is how a pod hides from
	// the orphan sweep).
	labels := copyStringMap(attempt.ExtraLabels)
	for key, value := range stampedLabels(cfg, attempt, runner) {
		labels[key] = value
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        PodName(attempt),
			Namespace:   cfg.Namespace,
			Labels:      labels,
			Annotations: copyStringMap(attempt.ExtraAnnotations),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        ptr.To(activeDeadlineSeconds(cfg, attempt)),
			AutomountServiceAccountToken: ptr.To(false),
			NodeSelector:                 map[string]string{NodeSelectorOSKey: nodeSelectorOS(runner.OS)},
		},
	}
	stampClassRestrictionsAnnotation(pod.Annotations, runner)
	stampIdentityAnnotations(pod.Annotations, attempt)

	stampResources(cfg, attempt, runner, &container, class, windows)
	stampVolumes(cfg, attempt, &pod.Spec, &container, class, windows)
	stampSecurity(&pod.Spec, &container, class, windows, admin)
	if windows {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, windowsTolerations()...)
	}

	pod.Spec.Containers = []corev1.Container{container}
	return pod, nil
}

// RenderFromTemplate instantiates the fresh stage pod from a consumer-
// authored Deployment's pod template (DI-9: template BY REFERENCE — sidecars,
// volumes, and node selectors stay under consumer control; the fresh/never-
// reused lifecycle and every dispatcher-owned stamp still hold). The
// template's FIRST container is taken as the stage container (the v1 reading
// of architecture §12 open point 2, stated rather than implied).
func RenderFromTemplate(cfg Config, attempt Attempt, runner RunnerSpec, deployment *appsv1.Deployment) (*corev1.Pod, error) {
	if deployment == nil {
		return nil, fmt.Errorf("dispatcher: runner %q names no readable template deployment", runner.Name)
	}
	if err := refuseOverrides(attempt); err != nil {
		return nil, err
	}
	if err := assertRestrictionsEnforced(attempt, runner); err != nil {
		return nil, err
	}
	admin, err := assertWindowsIdentity(attempt, runner)
	if err != nil {
		return nil, err
	}
	template := deployment.Spec.Template.DeepCopy()
	if len(template.Spec.Containers) == 0 {
		return nil, fmt.Errorf("dispatcher: template deployment %q for runner %q has no containers", deployment.Name, runner.Name)
	}
	// A template that tries to pre-set dispatcher-owned metadata is the same
	// escalation shape as workflow input: refused, not overwritten silently.
	for _, key := range []string{runnercap.LabelRunnerClass, runnercap.LabelRole} {
		if _, ok := template.Labels[key]; ok {
			return nil, &LabelOverrideError{Key: key, Source: fmt.Sprintf("template deployment %q", deployment.Name)}
		}
	}

	windows := runner.OS == osWindows
	class := restrictionSet(runner.Restrictions)

	labels := copyStringMap(template.Labels)
	for key, value := range attempt.ExtraLabels {
		labels[key] = value
	}
	for key, value := range stampedLabels(cfg, attempt, runner) {
		labels[key] = value
	}
	annotations := copyStringMap(template.Annotations)
	for key, value := range attempt.ExtraAnnotations {
		annotations[key] = value
	}

	spec := template.Spec.DeepCopy()
	spec.RestartPolicy = corev1.RestartPolicyNever
	spec.ActiveDeadlineSeconds = ptr.To(activeDeadlineSeconds(cfg, attempt))
	// Deny-first posture (dispatcher §2 item 3): a stage pod never calls the
	// API server, so it never mounts a ServiceAccount token — the same stamp
	// RenderPod applies. The template path must apply it too, or a deployment
	// whose template/SA leaves automount on yields a stage pod with a live
	// token, silently defeating the invariant the image path asserts.
	spec.AutomountServiceAccountToken = ptr.To(false)
	if spec.NodeSelector == nil {
		spec.NodeSelector = map[string]string{}
	}
	spec.NodeSelector[NodeSelectorOSKey] = nodeSelectorOS(runner.OS)
	if windows {
		spec.Tolerations = append(spec.Tolerations, windowsTolerations()...)
	}

	// Non-nil: the empty-container template was refused above.
	stage := stageContainerIn(spec.Containers)
	// Command/Args are dispatcher-owned in the template path too (#3699):
	// the disposal gate's surrender requirement applies uniformly to every
	// host kind, so whatever ENTRYPOINT/CMD the template's image declares is
	// overridden the same way RenderPod's fresh container is built — a
	// template controls sidecars/volumes/node selectors (DI-9), not whether
	// its own stage container actually runs the authored stage.
	stage.Command = []string{"goobers"}
	stage.Args = []string{DispatchExecCommand}
	// The consumer's template may declare its own container env (DI-9 owns
	// sidecars, volumes, node selectors AND the stage container's ambient
	// contract). Those names are ambient container variables in the pod, so
	// under env:default-deny the in-pod rebuild would drop them unless the
	// allowlist carries them — a template-declared var present on an
	// unrestricted class and silently gone on a restricted one. The consumer
	// Deployment is operator-authored infrastructure, the same trust level as
	// envPassthrough, so it is allowlisted rather than denied; a control-plane
	// name among them is still removed by the strip that runs after the
	// rebuild.
	templateDeclared := make([]string, 0, len(stage.Env))
	for _, e := range stage.Env {
		templateDeclared = append(templateDeclared, e.Name)
	}
	stage.Env = append(stage.Env, stageEnv(cfg, attempt, class, templateDeclared)...)
	stampResources(cfg, attempt, runner, stage, class, windows)
	stampVolumes(cfg, attempt, spec, stage, class, windows)
	// Security bindings stamp the STAGE container and the pod level; sidecar
	// containers keep the consumer's own settings — on Windows, a sidecar's
	// own windowsOptions.runAsUserName included (stampSecurity) — except the
	// fs restriction, which is a pod-wide effect and stamps every container.
	stampSecurity(spec, stage, class, windows, admin)
	if !windows && class[string(runnercap.RestrictionFSReadonly)] {
		for i := 1; i < len(spec.Containers); i++ {
			side := &spec.Containers[i]
			if side.SecurityContext == nil {
				side.SecurityContext = &corev1.SecurityContext{}
			}
			side.SecurityContext.ReadOnlyRootFilesystem = ptr.To(true)
		}
	}

	stampClassRestrictionsAnnotation(annotations, runner)
	stampIdentityAnnotations(annotations, attempt)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        PodName(attempt),
			Namespace:   cfg.Namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: *spec,
	}, nil
}

// stageContainerIn resolves the stage container within a rendered or templated
// pod spec's container list: the FIRST one, nil when there is none.
//
// This is DI-9's "a template controls sidecars/volumes/node selectors, not
// which container is the stage" rule, and it lives in one function because it
// is one rule. Both render paths honour it — RenderPod builds a pod with
// exactly one container, RenderFromTemplate takes the template's first
// container as the stage container — and the skew check and the report's image
// stamp then read the same container the render wrote to, by construction
// rather than by three sites agreeing. Index 0 rather than a name lookup: a
// consumer template whose first container is not called "stage" would
// otherwise silently resolve to nothing.
//
// Decision 002 Q2 / architecture §12 open point 2 names this first-container
// rule as an UNCLOSED architecture point. Routing every reader through here
// keeps the number of sites a future ruling has to change at one.
func stageContainerIn(containers []corev1.Container) *corev1.Container {
	if len(containers) == 0 {
		return nil
	}
	return &containers[0]
}

// templateStageImage returns the template's stage-container image for the
// skew check ("" when unreadable; the render itself re-validates shape).
func templateStageImage(deployment *appsv1.Deployment) string {
	if deployment == nil {
		return ""
	}
	if stage := stageContainerIn(deployment.Spec.Template.Spec.Containers); stage != nil {
		return stage.Image
	}
	return ""
}

// refuseOverrides is the §3 refuse-to-create for workflow/gaggle/stage input:
// no goobers.dev/* pod metadata may arrive from outside the dispatcher.
func refuseOverrides(attempt Attempt) error {
	for _, keys := range []map[string]string{attempt.ExtraLabels, attempt.ExtraAnnotations} {
		for key := range keys {
			if strings.HasPrefix(key, "goobers.dev/") {
				return &LabelOverrideError{Key: key, Source: "workflow/gaggle/stage input"}
			}
		}
	}
	return nil
}

func assertRestrictionsEnforced(attempt Attempt, runner RunnerSpec) error {
	enforced := restrictionSet(runner.Restrictions)
	var missing []string
	for _, required := range attempt.Restrictions {
		if !enforced[required] {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return &RestrictionMismatchError{Runner: runner.Name, Missing: missing}
	}
	return nil
}

// assertWindowsIdentity decides the Windows container identity of the
// attempt on runner, refusing the incoherent shapes (WindowsIdentityError).
// It returns admin=true exactly when the stage REQUIRES
// runnercap.CapabilityWindowsAdmin and the runner PROVIDES it — the one
// case stampSecurity renders ContainerAdministrator. On a Linux runner it
// only refuses a stage that requires the Windows privilege; the Linux
// identity model is untouched.
func assertWindowsIdentity(attempt Attempt, runner RunnerSpec) (admin bool, err error) {
	required := runnercap.HasWindowsAdmin(attempt.RunsOnCapabilities)
	windows := runner.OS == osWindows
	if required && !windows {
		return false, &WindowsIdentityError{Runner: runner.Name, Reason: fmt.Sprintf(
			"the stage requires %q (the ContainerAdministrator identity of a Windows stage pod) but the runner's os is %q — the solver should never have placed it here (#3619)",
			runnercap.CapabilityWindowsAdmin, runner.OS)}
	}
	if !windows {
		return false, nil
	}
	for _, effect := range runnercap.CanonicalRestrictions(runner.Restrictions) {
		if runnercap.KnownRestriction(effect) && !runnercap.DeclarableOnWindows(runnercap.Restriction(effect)) {
			return false, &WindowsIdentityError{Runner: runner.Name, Reason: fmt.Sprintf(
				"its class declares %q, which has no Windows binding in v1 — the pod would carry the class label and none of the isolation it names (restrictions doc D4/D11; the inventory loader refuses this entry)",
				effect)}
		}
	}
	provided := runnercap.HasWindowsAdmin(runner.Capabilities)
	if required && !provided {
		return false, &WindowsIdentityError{Runner: runner.Name, Reason: fmt.Sprintf(
			"the stage requires %q but the runner's provides.capabilities does not claim it — a stage that needs ContainerAdministrator is refused, never served as ContainerUser to fail with Access Denied, and never granted on a class that did not claim it (#3619)",
			runnercap.CapabilityWindowsAdmin)}
	}
	return required && provided, nil
}

func restrictionSet(restrictions []string) map[string]bool {
	set := make(map[string]bool, len(restrictions))
	for _, r := range restrictions {
		set[r] = true
	}
	return set
}

// stampedLabels are the dispatcher-owned labels every stage pod carries:
// exactly one runner-class label DERIVED from the resolved restriction set
// via the single shared producer (runnercap.RunnerClassValue, delivery
// decision 015), the role marker the baseline policies select on, the
// run/attempt identity the reconcile sweep keys on, and — when this
// dispatcher declares one — the owner the sweep scopes itself to.
func stampedLabels(cfg Config, attempt Attempt, runner RunnerSpec) map[string]string {
	labels := map[string]string{
		LabelManagedBy:             ManagedByValue,
		runnercap.LabelRole:        runnercap.RoleStage,
		runnercap.LabelRunnerClass: runnercap.RunnerClassValue(runner.Restrictions),
		LabelRun:                   sanitizeNameSegment(attempt.RunID, 63),
		LabelStage:                 sanitizeNameSegment(attempt.Stage, 63),
		LabelAttempt:               fmt.Sprintf("%d", attempt.Number),
	}
	// Absent owner stamps nothing rather than an "unknown" placeholder: a
	// placeholder is a value a second ownerless dispatcher would also match,
	// which is the cross-worker disposal this label exists to prevent. An
	// unlabeled pod is instead unreachable by any sweep, and SweepOrphans
	// refuses to run without an owner at all.
	if owner := cfg.ownerLabel(); owner != "" {
		labels[LabelOwner] = owner
	}
	return labels
}

// stampIdentityAnnotations records the attempt's verbatim run ID and stage
// name (see AnnotationRunID) and the workflow execution driving it (see
// AnnotationOwningWorkflowID). Both render paths call it, so a pod that the
// sweep can select is always a pod the sweep can address.
//
// An absent OwningWorkflowID stamps NOTHING rather than an empty value, for
// the same reason ownerLabel stamps nothing: a value the sweep would then
// have to special-case is a value it can get wrong, and podAttempt's
// "annotation missing" arm already means exactly "leave this pod alone".
func stampIdentityAnnotations(annotations map[string]string, attempt Attempt) {
	annotations[AnnotationRunID] = attempt.RunID
	annotations[AnnotationStage] = attempt.Stage
	if attempt.OwningWorkflowID != "" {
		annotations[AnnotationOwningWorkflowID] = attempt.OwningWorkflowID
	}
}

// stampClassRestrictionsAnnotation records the human-readable preimage of the
// runner-class label on the pod (runnercap.AnnotationRunnerClassRestrictions),
// so an opaque class value — or, more importantly, a pod that hangs at
// materialize because NO NetworkPolicy selects it (case A, a class with nothing
// rendered) — still names its restriction set at diagnosis without a preimage
// search. Both the value and this annotation derive from the same restriction
// set through runnercap.RunnerClassPreimage — the SAME function the per-class
// NetworkPolicy renderer stamps its annotation from — so the pod annotation and
// the policy annotation agree by construction, a mirror rather than a copy that
// can drift (round-trip asserted in the tests). Empty (unrestricted) writes
// nothing — the "unrestricted" label needs no preimage.
func stampClassRestrictionsAnnotation(annotations map[string]string, runner RunnerSpec) {
	if ann := runnercap.RunnerClassPreimage(runner.Restrictions); ann != "" {
		annotations[runnercap.AnnotationRunnerClassRestrictions] = ann
	}
}

func nodeSelectorOS(os string) string {
	if os == osWindows {
		return "windows"
	}
	return "linux"
}

func windowsTolerations() []corev1.Toleration {
	keys := []string{WindowsTolerationKey, WindowsTolerationKeyLegacy}
	tolerations := make([]corev1.Toleration, 0, len(keys))
	for _, key := range keys {
		tolerations = append(tolerations, corev1.Toleration{
			Key:      key,
			Operator: corev1.TolerationOpEqual,
			Value:    "windows",
			Effect:   corev1.TaintEffectNoSchedule,
		})
	}
	return tolerations
}

func activeDeadlineSeconds(cfg Config, attempt Attempt) int64 {
	deadline := attempt.stageTimeout() + cfg.deadlineMargin()
	return int64(deadline.Seconds())
}

// stageEnv renders the dispatcher-owned half of the stage container's
// environment. class is the RESOLVED RUNNER's enforced restriction set — the
// same map every other binding in this file reads — because one of those
// bindings (env:default-deny, #3725) is now an environment stamp rather than a
// volume or a security context.
//
// alreadyOnContainer names the variables the container ALREADY carries before
// this appends to it — the DI-9 template path's consumer-declared container
// env. They are not stamped here, but under env:default-deny the in-pod
// rebuild would drop them unless the allowlist names them, so they are threaded
// in rather than re-derived: only the caller knows what the template declared.
// The image path passes nil.
func stageEnv(cfg Config, attempt Attempt, class map[string]bool, alreadyOnContainer []string) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: EnvRunID, Value: attempt.RunID},
		{Name: EnvGaggle, Value: attempt.Gaggle},
		{Name: EnvWorkflow, Value: attempt.Workflow},
		{Name: EnvStage, Value: attempt.Stage},
		{Name: EnvAttempt, Value: fmt.Sprintf("%d", attempt.Number)},
	}
	if cfg.BlobEndpoint != "" {
		env = append(env, corev1.EnvVar{Name: EnvBlobEndpoint, Value: cfg.BlobEndpoint})
	}
	if cfg.WriteAPIBase != "" {
		env = append(env, corev1.EnvVar{Name: EnvDaemonAPI, Value: cfg.WriteAPIBase})
	}
	if attempt.PodToken != "" {
		env = append(env, corev1.EnvVar{Name: EnvPodToken, Value: attempt.PodToken})
	}
	if len(attempt.Command) > 0 {
		// []string always marshals; a marshal failure here would mean the Go
		// runtime itself is broken, not a data problem — ignoring the error
		// is standard for this exact shape (encoding/json's own doc example
		// does the same for json.Marshal([]string)).
		encoded, _ := json.Marshal(attempt.Command)
		env = append(env, corev1.EnvVar{Name: EnvStageCommand, Value: string(encoded)})
	}
	if attempt.Script != "" {
		env = append(env, corev1.EnvVar{Name: EnvStageScript, Value: attempt.Script})
	}
	env = append(env, corev1.EnvVar{Name: EnvStageTimeout, Value: attempt.stageTimeout().String()})
	for _, key := range sortedKeys(attempt.Env) {
		env = append(env, corev1.EnvVar{Name: key, Value: attempt.Env[key]})
	}
	// Declared inputs, named exactly as the local executor names them so a
	// stage reads GOOBERS_INPUT_<KEY> identically on both substrates.
	for _, key := range sortedKeys(attempt.Inputs) {
		env = append(env, corev1.EnvVar{Name: InputEnvVar(key), Value: attempt.Inputs[key]})
	}
	// Run context, for goobers-CLI stages ONLY. stageEnvironment() in the pod
	// strips the dispatcher's control plane, and these names overlap it — so
	// they are re-stamped here under a distinct prefix-free contract and
	// re-admitted in the pod only when the stage is a CLI stage.
	for _, key := range sortedKeys(attempt.RunContext) {
		env = append(env, corev1.EnvVar{Name: key, Value: attempt.RunContext[key]})
	}
	if attempt.CLIStage {
		env = append(env, corev1.EnvVar{Name: EnvStageIsCLI, Value: "true"})
	}
	if ws := strings.TrimSpace(attempt.Workspace); ws != "" {
		env = append(env, corev1.EnvVar{Name: EnvStageWorkspace, Value: ws})
	}
	if attempt.KitDigest != "" {
		env = append(env, corev1.EnvVar{Name: EnvAgenticKitDigest, Value: attempt.KitDigest})
	}
	if cap := attempt.CheckoutCapability; cap != "" {
		env = append(env, corev1.EnvVar{Name: EnvCheckoutCapability, Value: cap})
	}
	if attempt.WorkspaceDelta != "" {
		env = append(env, corev1.EnvVar{Name: EnvWorkspaceDelta, Value: attempt.WorkspaceDelta})
	}
	if len(attempt.Capabilities) > 0 {
		if encoded, err := json.Marshal(attempt.Capabilities); err == nil {
			env = append(env, corev1.EnvVar{Name: EnvStageCapabilities, Value: string(encoded)})
		}
	}
	// env:default-deny (#3725). Stamped ONLY for a class that enforces it, so
	// every other pod spec is byte-identical to before this existed.
	if class[string(runnercap.RestrictionEnvDefaultDeny)] {
		allow, _ := json.Marshal(stageEnvAllowlist(cfg, attempt, alreadyOnContainer))
		env = append(env,
			corev1.EnvVar{Name: EnvStageEnvDefaultDeny, Value: "true"},
			corev1.EnvVar{Name: EnvStageEnvAllow, Value: string(allow)},
		)
	}
	return env
}

// stageEnvAllowlist names the ambient container variables __dispatch-exec must
// keep when it applies env:default-deny: everything the DISPATCHER itself
// stamped for the stage above and everything a DI-9 template already declared
// on the stage container, plus the instance's operator-declared envPassthrough.
//
// The list is names only — never values — because the pod reads the values out
// of its own environment, where the container runtime already put them.
//
// The invariant it must satisfy, pinned by TestEveryStampedStageVarIsAllowlisted
// OrStripped: every name stageEnv() stamps is either in this list, in
// DispatcherControlEnv (stripped in the pod), or in procenv's own base. A name
// that is in none of the three is DELETED for a stage on a declaring class and
// present for the same stage on every other class — restriction-conditional
// breakage diagnosed at the far side, which is the #3725 shape itself. The
// run-identity names below fell exactly there in review: they are stamped in
// stageEnv's base block, a goobers-CLI stage keeps them by design, and without
// them here the in-pod rebuild deleted them before the CLI/non-CLI split ever
// ran — GOOBERS_RUN_ID/GOOBERS_WORKFLOW gone, so providerRunContext() fails
// closed and providers.BranchName cannot compose the run branch.
//
// Listing them is safe for a NON-CLI stage by the ordering property below:
// DispatcherControlEnv contains DispatcherRunIdentityEnv, and the strip runs
// after the rebuild, so a plain shell stage still loses them.
//
// It deliberately does NOT name GOOBERS_CRED_* or GOOBERS_REPO_*-by-prefix.
// Resolved credentials never pass through this filter at all: they are minted
// in-pod at stage start and appended to the command's environment AFTER it
// (dispatchexec.go). Naming them here would be an allowlist entry that looks
// load-bearing and is not — and a prefix grant for any image-baked GOOBERS_CRED_*
// besides. The run-context names (GOOBERS_REPO_* and friends) ARE listed, by
// exact name, because those the dispatcher really does stamp; the CLI/non-CLI
// control-plane split still decides whether a given stage keeps them.
//
// A control-plane name reaching this list would be re-admitted here and then
// removed again by the control-plane strip that runs after it, so the ordering
// in stageEnvironment() is what makes this list unable to re-admit the pod
// token BY NAME even if a stage declared `env: {GOOBERS_POD_TOKEN: ...}`.
// By NAME is the whole scope of that claim: kubelet expands $(VAR_NAME) inside
// a container env value against variables declared earlier in the same list, so
// a stage declaring `env: {X: "$(GOOBERS_POD_TOKEN)"}` receives the token's
// VALUE under a name this list legitimately allows. That by-value path predates
// this restriction — it leaks on an unrestricted class too, where nothing
// filters at all — and closing it means validating declared env values or
// reserving the GOOBERS_ prefix for env keys, which is its own change.
func stageEnvAllowlist(cfg Config, attempt Attempt, alreadyOnContainer []string) []string {
	names := make([]string, 0, len(attempt.Env)+len(attempt.Inputs)+len(attempt.RunContext)+len(cfg.EnvPassthrough)+len(DispatcherRunIdentityEnv)+len(alreadyOnContainer))
	names = append(names, sortedKeys(attempt.Env)...)
	for _, key := range sortedKeys(attempt.Inputs) {
		names = append(names, InputEnvVar(key))
	}
	names = append(names, sortedKeys(attempt.RunContext)...)
	// The run's operational identity, stamped in stageEnv's base block above.
	// A goobers-CLI stage keeps these; every other stage is stripped of them
	// by the control-plane strip that runs after the rebuild.
	names = append(names, DispatcherRunIdentityEnv...)
	names = append(names, alreadyOnContainer...)
	names = append(names, cfg.EnvPassthrough...)
	return names
}

// stampResources sets requests from the stage's runsOn minimums and limits
// from the runner ceiling (dsl-3.0.md D2), with the tmpfs budget rule of
// dispatcher §5: when a Linux tmp:ephemeral tmpfs is mounted, its explicit
// sizeLimit is ADDED to the container memory limit — memory-backed emptyDir
// usage counts against the limit, and a ceiling that never accounted for it
// turns a full /tmp into an unattributed OOM.
func stampResources(cfg Config, attempt Attempt, runner RunnerSpec, container *corev1.Container, class map[string]bool, windows bool) {
	requests := corev1.ResourceList{}
	for name, minimum := range map[corev1.ResourceName]string{
		corev1.ResourceCPU:              attempt.CPU,
		corev1.ResourceMemory:           attempt.Memory,
		corev1.ResourceEphemeralStorage: attempt.Disk,
	} {
		if minimum == "" {
			continue
		}
		if quantity, err := resource.ParseQuantity(minimum); err == nil {
			requests[name] = quantity
		}
	}
	limits := corev1.ResourceList{}
	for name, ceiling := range map[corev1.ResourceName]string{
		corev1.ResourceCPU:              runner.CPU,
		corev1.ResourceMemory:           runner.Memory,
		corev1.ResourceEphemeralStorage: runner.Disk,
	} {
		if ceiling == "" {
			continue
		}
		if quantity, err := resource.ParseQuantity(ceiling); err == nil {
			limits[name] = quantity
		}
	}
	if !windows && class[string(runnercap.RestrictionTmpEphemeral)] {
		if memory, ok := limits[corev1.ResourceMemory]; ok {
			budgeted := memory.DeepCopy()
			tmpfs := cfg.tmpfsSizeLimit()
			budgeted.Add(tmpfs)
			limits[corev1.ResourceMemory] = budgeted
		}
	}
	if len(requests) > 0 {
		container.Resources.Requests = requests
	}
	if len(limits) > 0 {
		container.Resources.Limits = limits
	}
}

// stampVolumes mounts the writable workspace (always), the tmp:ephemeral
// volume at the platform temp path (decision 006: Linux /tmp memory-backed
// with an EXPLICIT sizeLimit; Windows the profile-nested temp,
// node-disk-backed — memory-backed emptyDir is a Linux mechanism), and the
// writable HOME a Linux fs-readonly pod needs.
func stampVolumes(cfg Config, attempt Attempt, spec *corev1.PodSpec, container *corev1.Container, class map[string]bool, windows bool) {
	workspace := corev1.Volume{
		Name:         "workspace",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	// No per-volume SizeLimit on the workspace: the container's ephemeral-
	// storage LIMIT (runner.Disk, the ceiling — stampResources) already caps
	// total pod ephemeral use, and kubelet enforces an emptyDir sizeLimit
	// independently. Sizing this from attempt.Disk (the floor / request)
	// collapsed the usable workspace to the minimum and evicted the pod once
	// /workspace crossed it — declaring runsOn.disk paradoxically SHRANK
	// workspace. Leaving it nil lets the workspace grow to the container
	// ceiling, which is the intended cap.
	workspacePath := LinuxWorkspacePath
	if windows {
		workspacePath = WindowsWorkspacePath
	}
	spec.Volumes = append(spec.Volumes, workspace)
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "workspace", MountPath: workspacePath})
	// RUN THE STAGE *IN* THE WORKSPACE. Mounting it is not enough: without a
	// WorkingDir the stage inherits the image's default (/), which is not
	// writable by the non-root user the runner contract requires. A stage that
	// writes a relative path — the ordinary case — then fails with a message
	// naming the FILE, not the missing working directory:
	//
	//   tee: probe-exit-codes.txt: Permission denied
	//
	// which sends the reader looking at permissions on a file that was never
	// the problem. Observed on a live cluster before this line existed.
	container.WorkingDir = workspacePath

	if class[string(runnercap.RestrictionTmpEphemeral)] {
		tmpfs := cfg.tmpfsSizeLimit()
		tmp := corev1.Volume{
			Name: "tmp",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{
				SizeLimit: &tmpfs,
			}},
		}
		tmpPath := LinuxTmpPath
		if windows {
			tmpPath = WindowsTmpPath
		} else {
			tmp.EmptyDir.Medium = corev1.StorageMediumMemory
		}
		spec.Volumes = append(spec.Volumes, tmp)
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "tmp", MountPath: tmpPath})
		if windows {
			container.Env = append(container.Env,
				corev1.EnvVar{Name: "TMP", Value: tmpPath},
				corev1.EnvVar{Name: "TEMP", Value: tmpPath},
			)
		} else {
			container.Env = append(container.Env, corev1.EnvVar{Name: "TMPDIR", Value: tmpPath})
		}
	}

	if !windows && class[string(runnercap.RestrictionFSReadonly)] {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name:         "home",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: "home", MountPath: LinuxHomePath})
		container.Env = append(container.Env, corev1.EnvVar{Name: "HOME", Value: LinuxHomePath})
	}
}

// stampSecurity applies the restriction bindings by OS (decisions 006/007,
// restrictions doc D7/D8):
//
//   - Linux: the PSS-restricted baseline on every stage pod (runAsNonRoot,
//     RuntimeDefault seccomp, no privilege escalation, drop ALL), and
//     readOnlyRootFilesystem: true exactly when the class carries
//     fs:readonly-except-workspace.
//   - Windows: the container IDENTITY is the binding (#3619). Every Windows
//     stage pod is stamped windowsOptions.runAsUserName explicitly:
//     ContainerAdministrator when admin (the stage requires
//     runnercap.CapabilityWindowsAdmin and the class provides it —
//     assertWindowsIdentity), ContainerUser otherwise. The pre-#3619 shape
//     stamped ContainerUser only as the fs:readonly binding and let every
//     other Windows pod inherit the image's USER; now that an
//     admin-requiring stage has a declaration path, the identity is the
//     dispatcher's decision in BOTH directions, never the image's default.
//     The dispatcher MUST NOT stamp readOnlyRootFilesystem — Kubernetes
//     silently ignores it on Windows, which FAILS OPEN: a spec that says
//     readonly and a pod that is not (decision 007); fs:readonly is not
//     declarable on a Windows class at all (assertWindowsIdentity refuses
//     it). The Linux-only baseline fields are likewise not stamped (Windows
//     kubelets reject or ignore them).
func stampSecurity(spec *corev1.PodSpec, container *corev1.Container, class map[string]bool, windows, admin bool) {
	if windows {
		identity := WindowsRunAsUserName
		if admin {
			identity = WindowsAdminRunAsUserName
		}
		// Pod level, so the identity is legible on the pod and is the default
		// for every container a consumer template brought along that sets no
		// identity of its own; AND the stage container itself, because a
		// container-level runAsUserName wins over the pod-level one in
		// Kubernetes — a template whose stage container pre-set
		// ContainerAdministrator would otherwise override the dispatcher's
		// decision silently. A template SIDECAR that sets its own
		// runAsUserName keeps it (the container level wins there too): the
		// decision here is the STAGE's identity, and sidecars are
		// operator-owned infrastructure on the same trust root as
		// instance.yaml — the same boundary the Linux arm draws, which
		// stamps the PSS baseline on the stage container only.
		if spec.SecurityContext == nil {
			spec.SecurityContext = &corev1.PodSecurityContext{}
		}
		if spec.SecurityContext.WindowsOptions == nil {
			spec.SecurityContext.WindowsOptions = &corev1.WindowsSecurityContextOptions{}
		}
		spec.SecurityContext.WindowsOptions.RunAsUserName = ptr.To(identity)
		if container.SecurityContext == nil {
			container.SecurityContext = &corev1.SecurityContext{}
		}
		if container.SecurityContext.WindowsOptions == nil {
			container.SecurityContext.WindowsOptions = &corev1.WindowsSecurityContextOptions{}
		}
		container.SecurityContext.WindowsOptions.RunAsUserName = ptr.To(identity)
		return
	}
	spec.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot:   ptr.To(true),
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	if container.SecurityContext == nil {
		container.SecurityContext = &corev1.SecurityContext{}
	}
	container.SecurityContext.AllowPrivilegeEscalation = ptr.To(false)
	container.SecurityContext.Capabilities = &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}
	if class[string(runnercap.RestrictionFSReadonly)] {
		container.SecurityContext.ReadOnlyRootFilesystem = ptr.To(true)
	}
}

func copyStringMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// sortedKeys returns m's keys in sorted order, so a rendered pod's env list
// (and any test asserting on it) is deterministic despite Go's randomized
// map iteration.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// InputEnvVar renders a declared input key as its stage environment variable.
// This MUST match executor.InputEnvVar: the whole point is that a stage sees
// the same variable name whether it runs locally or in a pod. Duplicated
// rather than imported because internal/executor pulls in the local execution
// world the dispatcher deliberately does not depend on; the parity test is
// what keeps the two spellings honest.
func InputEnvVar(key string) string {
	sanitized := nonAlnumInputKey.ReplaceAllString(key, "_")
	return "GOOBERS_INPUT_" + strings.ToUpper(sanitized)
}

var nonAlnumInputKey = regexp.MustCompile(`[^A-Za-z0-9]+`)
