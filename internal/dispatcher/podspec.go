package dispatcher

import (
	"encoding/json"
	"fmt"
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
	// LinuxTmpPath / WindowsTmpPath is the platform temp path the
	// tmp:ephemeral volume mounts at (Linux /tmp; Windows the profile-nested
	// temp — decision 006).
	LinuxTmpPath   = "/tmp"
	WindowsTmpPath = `C:\Users\ContainerUser\AppData\Local\Temp`
)

// Node scheduling contract.
const (
	// NodeSelectorOSKey is the well-known node OS label.
	NodeSelectorOSKey = "kubernetes.io/os"
	// WindowsTolerationKey is the Windows node taint stage pods tolerate
	// (the upstream sig-windows convention: node.kubernetes.io/os=windows
	// :NoSchedule on Windows pools).
	WindowsTolerationKey = "node.kubernetes.io/os"
	// WindowsRunAsUserName is the non-admin Windows container identity the
	// fs restriction binds to (decision 007).
	WindowsRunAsUserName = "ContainerUser"
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

	windows := runner.OS == osWindows
	class := restrictionSet(runner.Restrictions)

	container := corev1.Container{
		Name:    StageContainerName,
		Image:   runner.Host,
		Command: []string{"goobers"},
		Args:    []string{DispatchExecCommand},
		Env:     stageEnv(cfg, attempt),
	}

	// Extra (non-goobers.dev) metadata merges FIRST; the dispatcher-owned
	// stamps land last and therefore always win — a workflow must not be able
	// to overwrite even the managed-by marker (that is how a pod hides from
	// the orphan sweep).
	labels := copyStringMap(attempt.ExtraLabels)
	for key, value := range stampedLabels(attempt, runner) {
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

	stampResources(cfg, attempt, runner, &container, class, windows)
	stampVolumes(cfg, attempt, &pod.Spec, &container, class, windows)
	stampSecurity(&pod.Spec, &container, class, windows)
	if windows {
		pod.Spec.Tolerations = append(pod.Spec.Tolerations, windowsToleration())
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
	for key, value := range stampedLabels(attempt, runner) {
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
		spec.Tolerations = append(spec.Tolerations, windowsToleration())
	}

	stage := &spec.Containers[0]
	// Command/Args are dispatcher-owned in the template path too (#3699):
	// the disposal gate's surrender requirement applies uniformly to every
	// host kind, so whatever ENTRYPOINT/CMD the template's image declares is
	// overridden the same way RenderPod's fresh container is built — a
	// template controls sidecars/volumes/node selectors (DI-9), not whether
	// its own stage container actually runs the authored stage.
	stage.Command = []string{"goobers"}
	stage.Args = []string{DispatchExecCommand}
	stage.Env = append(stage.Env, stageEnv(cfg, attempt)...)
	stampResources(cfg, attempt, runner, stage, class, windows)
	stampVolumes(cfg, attempt, spec, stage, class, windows)
	// Security bindings stamp the STAGE container and the pod level; sidecar
	// containers keep the consumer's own settings except the fs restriction,
	// which is a pod-wide effect and stamps every container.
	stampSecurity(spec, stage, class, windows)
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

// templateStageImage returns the template's stage-container image for the
// skew check ("" when unreadable; the render itself re-validates shape).
func templateStageImage(deployment *appsv1.Deployment) string {
	if deployment == nil || len(deployment.Spec.Template.Spec.Containers) == 0 {
		return ""
	}
	return deployment.Spec.Template.Spec.Containers[0].Image
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
// decision 015), the role marker the baseline policies select on, and the
// run/attempt identity the reconcile sweep keys on.
func stampedLabels(attempt Attempt, runner RunnerSpec) map[string]string {
	return map[string]string{
		LabelManagedBy:             ManagedByValue,
		runnercap.LabelRole:        runnercap.RoleStage,
		runnercap.LabelRunnerClass: runnercap.RunnerClassValue(runner.Restrictions),
		LabelRun:                   sanitizeNameSegment(attempt.RunID, 63),
		LabelStage:                 sanitizeNameSegment(attempt.Stage, 63),
		LabelAttempt:               fmt.Sprintf("%d", attempt.Number),
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

func windowsToleration() corev1.Toleration {
	return corev1.Toleration{
		Key:      WindowsTolerationKey,
		Operator: corev1.TolerationOpEqual,
		Value:    "windows",
		Effect:   corev1.TaintEffectNoSchedule,
	}
}

func activeDeadlineSeconds(cfg Config, attempt Attempt) int64 {
	deadline := attempt.stageTimeout() + cfg.deadlineMargin()
	return int64(deadline.Seconds())
}

func stageEnv(cfg Config, attempt Attempt) []corev1.EnvVar {
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
	return env
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
//   - Windows: the fs restriction binds to
//     windowsOptions.runAsUserName: ContainerUser, and the dispatcher MUST
//     NOT stamp readOnlyRootFilesystem — Kubernetes silently ignores it on
//     Windows, which FAILS OPEN: a spec that says readonly and a pod that
//     is not (decision 007). The Linux-only baseline fields are likewise
//     not stamped (Windows kubelets reject or ignore them).
func stampSecurity(spec *corev1.PodSpec, container *corev1.Container, class map[string]bool, windows bool) {
	if windows {
		// ContainerUser binds ONLY to fs:readonly-except-workspace (dispatcher
		// §5, AC-4), the Windows equivalent of readOnlyRootFilesystem. Stamping
		// it unconditionally would impose a non-admin identity on every Windows
		// stage — Access Denied for admin-requiring stages — and nothing asked
		// for it. In v1 no Windows pod carries fs:readonly (restrictions D4), so
		// this branch stamps nothing in v1; the binding is here for when it can.
		if class[string(runnercap.RestrictionFSReadonly)] {
			spec.SecurityContext = &corev1.PodSecurityContext{
				WindowsOptions: &corev1.WindowsSecurityContextOptions{
					RunAsUserName: ptr.To(WindowsRunAsUserName),
				},
			}
		}
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
