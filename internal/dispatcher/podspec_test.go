package dispatcher

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/procenv"
	"github.com/goobers/goobers/internal/runnercap"
)

func testConfig() Config {
	return Config{
		Namespace:       "gaggle-alpha",
		Owner:           "goobers-worker-0",
		EmbeddedCommit:  "0123456789abcdef0123456789abcdef01234567",
		EmbeddedVersion: "v0.1.0",
		BlobEndpoint:    "http://goobers-api.goobers-system:7777",
		WriteAPIBase:    "http://goobers-api.goobers-system:7777",
	}
}

func testAttempt() Attempt {
	return Attempt{
		RunID:    "run-2026-08-22-0001",
		Gaggle:   "alpha",
		Workflow: "implementation",
		Stage:    "build",
		Number:   1,
		CPU:      "500m",
		Memory:   "1Gi",
		Disk:     "10Gi",
		PodToken: "goobers-pod.tok",
		// Deliberately NOT the run id and not composable from it: the fixture
		// carries the SCHEDULED shape (ClaimScheduled's child, claimID+"-run",
		// against a RunID the engine rewrote to a hash), because that is the
		// shape a sweep composing ids from the run/stage/attempt gets wrong.
		OwningWorkflowID: "goobers-e2e-nightly-2026-08-22T03:00:00Z-run",
	}
}

func linuxRunner() RunnerSpec {
	return RunnerSpec{
		Name:         "tiny-linux",
		OS:           "linux",
		HostKind:     instance.RunnerHostImage,
		Host:         "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
		CPU:          "2000m",
		Memory:       "4Gi",
		Disk:         "20Gi",
		Restrictions: []string{"network:allowlist", "fs:readonly-except-workspace", "tmp:ephemeral"},
	}
}

func windowsRunner() RunnerSpec {
	return RunnerSpec{
		Name:     "win-large",
		OS:       "windows",
		HostKind: instance.RunnerHostImage,
		Host:     "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
		CPU:      "4000m",
		Memory:   "8Gi",
	}
}

// §8 item 2 (half 1): the runner-class label on every created pod is DERIVED
// from the resolved restriction set through the single shared producer —
// exactly one class label, plus the role marker the baseline policies select
// on. This is also the decision-015 ROUND-TRIP: the value the dispatcher
// stamps equals the value a rendered per-class NetworkPolicy selector derives
// from the same inventory restriction set, because both call
// runnercap.RunnerClassValue.
func TestRenderPodStampsDerivedRunnerClassLabel(t *testing.T) {
	pod, err := RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	want := runnercap.RunnerClassValue(linuxRunner().Restrictions)
	if got := pod.Labels[runnercap.LabelRunnerClass]; got != want {
		t.Fatalf("runner-class label = %q, want the shared derivation %q", got, want)
	}
	// The renderer side of the contract: a policy selector built from the
	// inventory's restriction set (any declaration order) selects this pod.
	selectorValue := runnercap.RunnerClassValue([]string{
		"tmp:ephemeral", "fs:readonly-except-workspace", "network:allowlist",
	})
	if pod.Labels[runnercap.LabelRunnerClass] != selectorValue {
		t.Fatalf("stamped class %q does not round-trip to the selector derivation %q — the decision-015 contract is broken",
			pod.Labels[runnercap.LabelRunnerClass], selectorValue)
	}
	if got := pod.Labels[runnercap.LabelRole]; got != runnercap.RoleStage {
		t.Fatalf("role label = %q, want %q", got, runnercap.RoleStage)
	}
}

// §8 item 2 (half 2): a workflow attempting to set the class label — or
// anything in the dispatcher-owned goobers.dev/ metadata namespace — is
// refused AT DISPATCH, not silently overwritten.
func TestRenderPodRefusesLabelOverride(t *testing.T) {
	for _, key := range []string{runnercap.LabelRunnerClass, runnercap.LabelRole, "goobers.dev/anything"} {
		attempt := testAttempt()
		attempt.ExtraLabels = map[string]string{key: "attacker-chosen"}
		_, err := RenderPod(testConfig(), attempt, linuxRunner())
		var override *LabelOverrideError
		if !errors.As(err, &override) {
			t.Fatalf("ExtraLabels[%q]: got err %v, want LabelOverrideError", key, err)
		}
		if override.Key != key {
			t.Fatalf("LabelOverrideError.Key = %q, want %q", override.Key, key)
		}
	}
	// Annotations are the same escalation surface.
	attempt := testAttempt()
	attempt.ExtraAnnotations = map[string]string{runnercap.LabelRunnerClass: "x"}
	if _, err := RenderPod(testConfig(), attempt, linuxRunner()); err == nil {
		t.Fatal("goobers.dev/ annotation override was not refused")
	}
	// Non-goobers.dev metadata passes through untouched.
	attempt = testAttempt()
	attempt.ExtraLabels = map[string]string{"team.example.com/cost-center": "42"}
	pod, err := RenderPod(testConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("benign extra label refused: %v", err)
	}
	if pod.Labels["team.example.com/cost-center"] != "42" {
		t.Fatal("benign extra label dropped")
	}
	// Dispatcher-owned stamps outside goobers.dev/ (the managed-by marker)
	// are not refused but must WIN the merge — overwriting managed-by is how
	// a pod would hide from the orphan sweep.
	attempt = testAttempt()
	attempt.ExtraLabels = map[string]string{LabelManagedBy: "someone-else"}
	pod, err = RenderPod(testConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if pod.Labels[LabelManagedBy] != ManagedByValue {
		t.Fatalf("managed-by = %q — workflow input overwrote the sweep marker", pod.Labels[LabelManagedBy])
	}
}

// Refuse-to-create on restriction mismatch: the stage's effective requirement
// must be within what the resolved runner enforces (restrictions doc §6:
// asserted at dispatch).
func TestRenderPodRefusesRestrictionMismatch(t *testing.T) {
	attempt := testAttempt()
	attempt.Restrictions = []string{"network:none"}
	_, err := RenderPod(testConfig(), attempt, linuxRunner())
	var mismatch *RestrictionMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("got err %v, want RestrictionMismatchError", err)
	}
	if len(mismatch.Missing) != 1 || mismatch.Missing[0] != "network:none" {
		t.Fatalf("Missing = %v, want [network:none]", mismatch.Missing)
	}
}

// §8 item 4: on a Windows pod, readOnlyRootFilesystem is NOT stamped —
// Kubernetes silently ignores it on Windows, which fails OPEN (decision 007).
// A v1 Windows pod carries no restrictions (D4), so it gets NO ContainerUser
// either — ContainerUser is the fs:readonly binding, not a Windows default —
// and the Linux-only baseline fields are absent.
func TestRenderPodWindowsDoesNotStampReadOnlyRootFilesystem(t *testing.T) {
	runner := windowsRunner()
	runner.Restrictions = nil // Windows declares no restrictions in v1 (D4)
	pod, err := RenderPod(testConfig(), testAttempt(), runner)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	container := pod.Spec.Containers[0]
	if container.SecurityContext != nil && container.SecurityContext.ReadOnlyRootFilesystem != nil {
		t.Fatal("readOnlyRootFilesystem stamped on a Windows pod — silently ignored by Kubernetes, fails OPEN (decision 007)")
	}
	// No fs:readonly in the class → NO ContainerUser. ContainerUser binds only
	// to fs:readonly-except-workspace (dispatcher §5); stamping it on a plain
	// Windows stage imposes a non-admin identity the stage never asked for and
	// admin-requiring stages hit Access Denied at cutover.
	if sc := pod.Spec.SecurityContext; sc != nil && sc.WindowsOptions != nil && sc.WindowsOptions.RunAsUserName != nil {
		t.Fatalf("ContainerUser stamped on a Windows pod with no fs:readonly (runAsUserName=%q)", *sc.WindowsOptions.RunAsUserName)
	}
	if sc := pod.Spec.SecurityContext; sc != nil && (sc.RunAsNonRoot != nil || sc.SeccompProfile != nil) {
		t.Fatal("Linux-only securityContext fields stamped on a Windows pod")
	}
	if got := pod.Spec.NodeSelector[NodeSelectorOSKey]; got != "windows" {
		t.Fatalf("node selector %s = %q, want windows", NodeSelectorOSKey, got)
	}
	found := false
	for _, toleration := range pod.Spec.Tolerations {
		if toleration.Key == WindowsTolerationKey && toleration.Value == "windows" {
			found = true
		}
	}
	if !found {
		t.Fatal("Windows toleration missing")
	}
}

// The Windows fs:readonly binding (the positive case): a Windows pod whose
// class DOES carry fs:readonly-except-workspace gets ContainerUser (the Windows
// equivalent of readOnlyRootFilesystem) — but STILL not readOnlyRootFilesystem
// itself, which fails open on Windows (decision 007). The v1 solver won't place
// fs:readonly on Windows (D4); this pins the renderer's binding regardless, so
// the gate is proven in both directions.
func TestRenderPodWindowsContainerUserGatedOnFSReadonly(t *testing.T) {
	runner := windowsRunner()
	runner.Restrictions = []string{"fs:readonly-except-workspace"}
	pod, err := RenderPod(testConfig(), testAttempt(), runner)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	sc := pod.Spec.SecurityContext
	if sc == nil || sc.WindowsOptions == nil || sc.WindowsOptions.RunAsUserName == nil ||
		*sc.WindowsOptions.RunAsUserName != WindowsRunAsUserName {
		t.Fatal("ContainerUser not stamped on a Windows pod carrying fs:readonly-except-workspace")
	}
	if c := pod.Spec.Containers[0]; c.SecurityContext != nil && c.SecurityContext.ReadOnlyRootFilesystem != nil {
		t.Fatal("readOnlyRootFilesystem stamped on a Windows pod — fails OPEN (decision 007), even with fs:readonly present")
	}
}

// The Linux counterpart: fs:readonly-except-workspace stamps
// readOnlyRootFilesystem PLUS a writable workspace and writable HOME, and the
// PSS-restricted baseline is present.
func TestRenderPodLinuxReadonlyBinding(t *testing.T) {
	pod, err := RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	container := pod.Spec.Containers[0]
	if container.SecurityContext == nil || container.SecurityContext.ReadOnlyRootFilesystem == nil || !*container.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("readOnlyRootFilesystem not stamped for the Linux fs restriction")
	}
	mounts := map[string]string{}
	for _, mount := range container.VolumeMounts {
		mounts[mount.Name] = mount.MountPath
	}
	if mounts["workspace"] != LinuxWorkspacePath {
		t.Fatalf("workspace mount = %q, want %q", mounts["workspace"], LinuxWorkspacePath)
	}
	if mounts["home"] != LinuxHomePath {
		t.Fatalf("home mount = %q, want %q (readOnlyRootFilesystem needs a writable HOME)", mounts["home"], LinuxHomePath)
	}
	var home string
	for _, env := range container.Env {
		if env.Name == "HOME" {
			home = env.Value
		}
	}
	if home != LinuxHomePath {
		t.Fatalf("HOME env = %q, want %q", home, LinuxHomePath)
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsNonRoot == nil || !*pod.Spec.SecurityContext.RunAsNonRoot {
		t.Fatal("runAsNonRoot baseline missing on Linux pod")
	}
	if pod.Spec.SecurityContext.SeccompProfile == nil || pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("RuntimeDefault seccomp baseline missing on Linux pod")
	}
}

// §8 item 6 mechanics: the tmp:ephemeral tmpfs carries an EXPLICIT sizeLimit
// (never the half-node-RAM default) and that sizeLimit is ADDED INTO the
// container memory limit, so filling /tmp fails with a named limit rather
// than an unattributed OOM against a ceiling that never accounted for it.
func TestRenderPodBudgetsTmpfsIntoMemoryLimit(t *testing.T) {
	cfg := testConfig()
	cfg.TmpfsSizeLimit = resource.MustParse("1Gi")
	pod, err := RenderPod(cfg, testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	var tmp *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "tmp" {
			tmp = &pod.Spec.Volumes[i]
		}
	}
	if tmp == nil || tmp.EmptyDir == nil {
		t.Fatal("tmp:ephemeral volume missing")
	}
	if tmp.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Fatalf("tmp medium = %q, want Memory on Linux", tmp.EmptyDir.Medium)
	}
	if tmp.EmptyDir.SizeLimit == nil || tmp.EmptyDir.SizeLimit.Cmp(resource.MustParse("1Gi")) != 0 {
		t.Fatalf("tmp sizeLimit = %v, want explicit 1Gi", tmp.EmptyDir.SizeLimit)
	}
	limits := pod.Spec.Containers[0].Resources.Limits
	// Runner ceiling 4Gi + tmpfs 1Gi = 5Gi.
	if got, want := limits[corev1.ResourceMemory], resource.MustParse("5Gi"); got.Cmp(want) != 0 {
		t.Fatalf("memory limit = %s, want %s (ceiling 4Gi + tmpfs 1Gi budgeted in)", got.String(), want.String())
	}
	// Requests come from the stage minimums, limits from the runner ceiling.
	requests := pod.Spec.Containers[0].Resources.Requests
	if got, want := requests[corev1.ResourceMemory], resource.MustParse("1Gi"); got.Cmp(want) != 0 {
		t.Fatalf("memory request = %s, want the stage minimum %s", got.String(), want.String())
	}
	if got, want := limits[corev1.ResourceCPU], resource.MustParse("2000m"); got.Cmp(want) != 0 {
		t.Fatalf("cpu limit = %s, want the runner ceiling %s", got.String(), want.String())
	}
}

// The runner-class annotation is a non-drifting mirror of the label on the pod
// itself: the stamped annotation, split back into a restriction set, hashes to
// the stamped label value — so at 2am a pod that no NetworkPolicy selects (the
// hard case: nothing to read on the netpol side) still names its restriction
// set, and the preimage cannot lie without failing this test. An unrestricted
// pod carries no annotation (its "unrestricted" label needs none).
func TestRenderPodClassAnnotationMirrorsLabel(t *testing.T) {
	pod, err := RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	label := pod.Labels[runnercap.LabelRunnerClass]
	ann := pod.Annotations[runnercap.AnnotationRunnerClassRestrictions]
	if ann == "" {
		t.Fatal("runner-class-restrictions annotation not stamped on a restricted pod")
	}
	if got := runnercap.RunnerClassValue(strings.Split(ann, ",")); got != label {
		t.Fatalf("annotation %q re-derives to %q, want the stamped label %q", ann, got, label)
	}
	bare := linuxRunner()
	bare.Restrictions = nil
	pod, err = RenderPod(testConfig(), testAttempt(), bare)
	if err != nil {
		t.Fatalf("RenderPod (unrestricted): %v", err)
	}
	if _, ok := pod.Annotations[runnercap.AnnotationRunnerClassRestrictions]; ok {
		t.Fatal("unrestricted pod should carry no runner-class-restrictions annotation")
	}
	if got := pod.Labels[runnercap.LabelRunnerClass]; got != runnercap.RunnerClassUnrestricted {
		t.Fatalf("unrestricted label = %q, want %q", got, runnercap.RunnerClassUnrestricted)
	}
}

// The workspace emptyDir carries NO per-volume sizeLimit: capping it from
// attempt.Disk (the floor / request, 10Gi) collapsed usable workspace to the
// minimum and evicted the pod the moment /workspace crossed it — so declaring
// runsOn.disk paradoxically SHRANK workspace. The container ephemeral-storage
// LIMIT (the runner ceiling, 20Gi) governs total pod ephemeral use instead.
func TestRenderPodWorkspaceHasNoFloorSizeLimit(t *testing.T) {
	pod, err := RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	var workspace *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "workspace" {
			workspace = &pod.Spec.Volumes[i]
		}
	}
	if workspace == nil || workspace.EmptyDir == nil {
		t.Fatal("workspace emptyDir volume missing")
	}
	if workspace.EmptyDir.SizeLimit != nil {
		t.Fatalf("workspace sizeLimit = %v, want nil (the container ephemeral ceiling governs, not the floor)", workspace.EmptyDir.SizeLimit)
	}
	if limit := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceEphemeralStorage]; limit.Cmp(resource.MustParse("20Gi")) != 0 {
		t.Fatalf("ephemeral-storage limit = %s, want the runner ceiling 20Gi", limit.String())
	}
}

// A dispatcher with no configured tmpfs size still stamps an explicit
// sizeLimit — the default is a named constant, never Kubernetes' half-node
// fallback.
func TestRenderPodTmpfsSizeLimitAlwaysExplicit(t *testing.T) {
	pod, err := RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "tmp" {
			if volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.Cmp(DefaultTmpfsSizeLimit) != 0 {
				t.Fatalf("tmp sizeLimit = %v, want the explicit default %s", volume.EmptyDir.SizeLimit, DefaultTmpfsSizeLimit.String())
			}
			return
		}
	}
	t.Fatal("tmp volume missing")
}

// The always-on orphan backstop: every pod carries activeDeadlineSeconds =
// stage timeout + margin, including stages that declared no timeout.
func TestRenderPodActiveDeadlineAlwaysOn(t *testing.T) {
	attempt := testAttempt()
	attempt.Timeout = 30 * time.Minute
	pod, err := RenderPod(testConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("activeDeadlineSeconds missing")
	}
	want := int64((30*60 + 10*60)) // 30m stage + 10m default margin
	if *pod.Spec.ActiveDeadlineSeconds != want {
		t.Fatalf("activeDeadlineSeconds = %d, want %d", *pod.Spec.ActiveDeadlineSeconds, want)
	}

	// No declared timeout: still finite.
	attempt.Timeout = 0
	pod, err = RenderPod(testConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if pod.Spec.ActiveDeadlineSeconds == nil || *pod.Spec.ActiveDeadlineSeconds <= 0 {
		t.Fatal("a stage with no declared timeout must still get a finite activeDeadlineSeconds")
	}
}

// The pod-plane env contract: identity, blob endpoint (decision 010 — every
// class carries the blob data path), write API, and the per-run bearer.
func TestRenderPodEnvContract(t *testing.T) {
	pod, err := RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	for name, want := range map[string]string{
		EnvRunID:        "run-2026-08-22-0001",
		EnvStage:        "build",
		EnvAttempt:      "1",
		EnvBlobEndpoint: "http://goobers-api.goobers-system:7777",
		EnvDaemonAPI:    "http://goobers-api.goobers-system:7777",
		EnvPodToken:     "goobers-pod.tok",
		EnvStageTimeout: DefaultStageTimeout.String(),
	} {
		if env[name] != want {
			t.Errorf("env %s = %q, want %q", name, env[name], want)
		}
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("stage pods must not automount the ServiceAccount token (deny-first posture; stage pods never call the API server)")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never (one attempt per pod)", pod.Spec.RestartPolicy)
	}
}

// #3699: a rendered pod must run the authored stage, not the image's own
// ENTRYPOINT/CMD — this is the core assertion the bug's absence of a test
// let slip through originally.
func TestRenderPodExecutesTheAuthoredStage(t *testing.T) {
	pod, err := RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	container := pod.Spec.Containers[0]
	if len(container.Command) != 1 || container.Command[0] != "goobers" {
		t.Fatalf("container.Command = %v, want [\"goobers\"]", container.Command)
	}
	if len(container.Args) != 1 || container.Args[0] != DispatchExecCommand {
		t.Fatalf("container.Args = %v, want [%q]", container.Args, DispatchExecCommand)
	}
}

// GOOBERS_STAGE_COMMAND/GOOBERS_STAGE_SCRIPT carry whichever of
// Attempt.Command/Script is set (mutually exclusive, mirroring
// DeterministicRun), and Attempt.Env entries land as their own EnvVars.
func TestRenderPodStampsCommandScriptAndEnv(t *testing.T) {
	withCommand := testAttempt()
	withCommand.Command = []string{"make", "ci"}
	withCommand.Env = map[string]string{"GOOBERS_PROBE_TARGET": "8.8.8.8", "GOOBERS_PROBE_MODE": "egress"}
	pod, err := RenderPod(testConfig(), withCommand, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if want := `["make","ci"]`; env[EnvStageCommand] != want {
		t.Errorf("%s = %q, want %q", EnvStageCommand, env[EnvStageCommand], want)
	}
	if _, ok := env[EnvStageScript]; ok {
		t.Errorf("%s must be absent when Command is set", EnvStageScript)
	}
	if env["GOOBERS_PROBE_TARGET"] != "8.8.8.8" || env["GOOBERS_PROBE_MODE"] != "egress" {
		t.Errorf("Attempt.Env did not land verbatim: %+v", env)
	}

	withScript := testAttempt()
	withScript.Script = "curl -sf https://example.invalid"
	pod, err = RenderPod(testConfig(), withScript, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	env = map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env[EnvStageScript] != withScript.Script {
		t.Errorf("%s = %q, want %q", EnvStageScript, env[EnvStageScript], withScript.Script)
	}
	if _, ok := env[EnvStageCommand]; ok {
		t.Errorf("%s must be absent when Script is set", EnvStageCommand)
	}

	// The baseline fixture declares neither: both stay absent rather than
	// stamping an empty/null placeholder.
	pod, err = RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == EnvStageCommand || e.Name == EnvStageScript {
			t.Errorf("unexpected %s on a Command/Script-less attempt", e.Name)
		}
	}
}

// RenderFromTemplate must run the authored stage too (#3699): the disposal
// gate's surrender requirement is uniform across host kinds, so the
// template's own ENTRYPOINT/CMD is overridden the same way RenderPod's fresh
// container is built, exactly like the dispatcher-owned labels already are.
func TestRenderFromTemplateExecutesTheAuthoredStage(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer-template"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:    "stage",
						Image:   "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
						Command: []string{"/bin/sh"},
						Args:    []string{"-c", "sleep infinity"},
					}},
				},
			},
		},
	}
	pod, err := RenderFromTemplate(testConfig(), testAttempt(), linuxRunner(), deployment)
	if err != nil {
		t.Fatalf("RenderFromTemplate: %v", err)
	}
	container := pod.Spec.Containers[0]
	if len(container.Command) != 1 || container.Command[0] != "goobers" {
		t.Fatalf("template container.Command = %v, want [\"goobers\"] (dispatcher-owned, not the template's)", container.Command)
	}
	if len(container.Args) != 1 || container.Args[0] != DispatchExecCommand {
		t.Fatalf("template container.Args = %v, want [%q]", container.Args, DispatchExecCommand)
	}
}

// Fresh-pod-per-attempt naming: distinct attempts of one stage produce
// distinct pod names; the same attempt redelivered produces the SAME name (so
// a duplicate create collides instead of double-running).
func TestPodNamePerAttempt(t *testing.T) {
	first := testAttempt()
	second := testAttempt()
	second.Number = 2
	if PodName(first) == PodName(second) {
		t.Fatalf("attempts 1 and 2 share pod name %q — a new attempt must be a new pod", PodName(first))
	}
	if PodName(first) != PodName(testAttempt()) {
		t.Fatal("the same attempt must derive the same pod name (idempotent create)")
	}
	long := testAttempt()
	long.Stage = "an-extremely-long-stage-name-that-would-overflow-the-dns-label-budget"
	long.RunID = strings.Repeat("r", 100)
	name := PodName(long)
	if len(name) > 63 {
		t.Fatalf("pod name %q exceeds the 63-character DNS label bound", name)
	}
}

// DI-9: the deployment host kind instantiates the consumer's template —
// sidecars and consumer volumes survive — while every dispatcher-owned stamp
// (identity labels, class label, deadline, restart policy, env) still lands,
// and a template that pre-sets dispatcher-owned labels is refused.
func TestRenderFromTemplate(t *testing.T) {
	template := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer-runner"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"consumer.example.com/tier": "fat"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "stage", Image: "ghcr.io/consumer/fat:0123456789abcdef0123456789abcdef01234567"},
						{Name: "sidecar", Image: "ghcr.io/consumer/logging:1"},
					},
				},
			},
		},
	}
	runner := RunnerSpec{
		Name: "consumer", OS: "linux", HostKind: instance.RunnerHostDeployment,
		Host: "consumer-runner", Memory: "4Gi",
		Restrictions: []string{"fs:readonly-except-workspace"},
	}
	pod, err := RenderFromTemplate(testConfig(), testAttempt(), runner, template)
	if err != nil {
		t.Fatalf("RenderFromTemplate: %v", err)
	}
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("template sidecar lost: %d containers", len(pod.Spec.Containers))
	}
	if pod.Labels["consumer.example.com/tier"] != "fat" {
		t.Fatal("consumer template label lost")
	}
	if pod.Labels[runnercap.LabelRunnerClass] != runnercap.RunnerClassValue(runner.Restrictions) {
		t.Fatal("derived runner-class label missing on template-instantiated pod")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("template instantiation must force restartPolicy Never (fresh pod per attempt)")
	}
	if pod.Spec.ActiveDeadlineSeconds == nil {
		t.Fatal("template instantiation must stamp the activeDeadlineSeconds backstop")
	}
	// Deny-first (DI-9): the template path must disable SA-token automount just
	// like the image path, or a deployment whose template/SA leaves automount
	// on yields a stage pod with a live token. The template above sets it nil.
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("template instantiation must stamp AutomountServiceAccountToken=false (deny-first posture)")
	}
	// The fs restriction is pod-wide: the sidecar gets readOnlyRootFilesystem
	// too.
	side := pod.Spec.Containers[1]
	if side.SecurityContext == nil || side.SecurityContext.ReadOnlyRootFilesystem == nil || !*side.SecurityContext.ReadOnlyRootFilesystem {
		t.Fatal("fs restriction not stamped on the template's sidecar container")
	}

	// A template pre-setting the class label is the same escalation as
	// workflow input: refused.
	template.Spec.Template.Labels[runnercap.LabelRunnerClass] = "general"
	if _, err := RenderFromTemplate(testConfig(), testAttempt(), runner, template); err == nil {
		t.Fatal("template-supplied runner-class label was not refused")
	}
}

// A mounted workspace the stage does not run in is not a workspace. Without
// WorkingDir the stage inherits the image default (/), which the non-root
// runner contract cannot write — and the failure names the file rather than
// the directory, so it reads as a permissions bug on the wrong object.
func TestRenderSetsWorkingDirToTheWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name string
		os   string
		want string
	}{
		{name: "linux", os: "linux", want: LinuxWorkspacePath},
		{name: "windows", os: "windows", want: WindowsWorkspacePath},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := linuxRunner()
			runner.OS = tc.os
			pod, err := RenderPod(testConfig(), testAttempt(), runner)
			if err != nil {
				t.Fatalf("RenderPod: %v", err)
			}
			c := pod.Spec.Containers[0]
			if c.WorkingDir != tc.want {
				t.Fatalf("WorkingDir = %q, want %q — the stage must RUN in the workspace, not merely have it mounted", c.WorkingDir, tc.want)
			}
			var mounted bool
			for _, m := range c.VolumeMounts {
				if m.Name == "workspace" && m.MountPath == tc.want {
					mounted = true
				}
			}
			if !mounted {
				t.Fatalf("workspace is not mounted at %s; WorkingDir would point at nothing", tc.want)
			}
		})
	}
}

// A Windows stage pod must tolerate BOTH Windows node-taint conventions. AKS
// applies kubernetes.io/os=windows:NoSchedule (the same key as the OS label);
// sig-windows documents node.kubernetes.io/os. Tolerating only one leaves the
// pod Pending on clusters using the other, with the node selector correctly
// targeting a node the pod may not land on — a failure that looks like
// capacity, not configuration.
func TestWindowsStagePodToleratesBothTaintConventions(t *testing.T) {
	runner := linuxRunner()
	runner.OS = "windows"
	pod, err := RenderPod(testConfig(), testAttempt(), runner)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	want := map[string]bool{WindowsTolerationKey: false, WindowsTolerationKeyLegacy: false}
	for _, tol := range pod.Spec.Tolerations {
		if tol.Value == "windows" && tol.Effect == corev1.TaintEffectNoSchedule {
			if _, ok := want[tol.Key]; ok {
				want[tol.Key] = true
			}
		}
	}
	for key, found := range want {
		if !found {
			t.Fatalf("windows stage pod does not tolerate %q; tolerations=%+v", key, pod.Spec.Tolerations)
		}
	}

	// And a linux pod must tolerate neither — the stamp is OS-conditional.
	linux, err := RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod(linux): %v", err)
	}
	for _, tol := range linux.Spec.Tolerations {
		if tol.Value == "windows" {
			t.Fatalf("linux stage pod must not carry a windows toleration, got %+v", tol)
		}
	}
}

// Declared inputs must reach the pod as GOOBERS_INPUT_<KEY>. Without this a
// pod-executed stage saw every input unset while the identical stage on the
// self runner saw them all — and nothing reported a difference.
func TestStagePodCarriesDeclaredInputs(t *testing.T) {
	attempt := testAttempt()
	attempt.Inputs = map[string]string{"resultFile": "parity.json", "probe-value": "42"}
	pod, err := RenderPod(testConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	got := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		got[e.Name] = e.Value
	}
	if got["GOOBERS_INPUT_RESULTFILE"] != "parity.json" {
		t.Fatalf("GOOBERS_INPUT_RESULTFILE = %q, want parity.json (env=%v)", got["GOOBERS_INPUT_RESULTFILE"], got)
	}
	// Non-alphanumerics are sanitized to underscore, upper-cased — the local
	// executor's rule, which this must match exactly.
	if got["GOOBERS_INPUT_PROBE_VALUE"] != "42" {
		t.Fatalf("GOOBERS_INPUT_PROBE_VALUE = %q, want 42 (env=%v)", got["GOOBERS_INPUT_PROBE_VALUE"], got)
	}
}

// The dispatcher duplicates executor.InputEnvVar rather than importing it (the
// dispatcher deliberately does not depend on the local execution world). This
// pins the two spellings together: if they drift, a stage reads its inputs
// under one name locally and another in a pod, and no error says so.
func TestInputEnvVarMatchesTheLocalExecutorSpelling(t *testing.T) {
	for _, key := range []string{"resultFile", "probe-value", "a.b c", "SINCE", "x"} {
		if got, want := InputEnvVar(key), executorInputEnvVarReference(key); got != want {
			t.Fatalf("InputEnvVar(%q) = %q, local executor spells it %q", key, got, want)
		}
	}
}

// executorInputEnvVarReference restates internal/executor.InputEnvVar. Kept
// here as a literal transcription so this test fails if either implementation
// changes without the other.
func executorInputEnvVarReference(key string) string {
	sanitized := regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(key, "_")
	return "GOOBERS_INPUT_" + strings.ToUpper(sanitized)
}

// Capability NAMES travel on the pod spec; values never do. A pod spec is
// readable by anyone with get-pod in the namespace, so a resolved secret on it
// would be a credential leak with a very wide blast radius.
func TestStagePodCarriesCapabilityNamesButNeverValues(t *testing.T) {
	attempt := testAttempt()
	attempt.Capabilities = []string{"contents:write", "issues:write"}
	pod, err := RenderPod(testConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	var found string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == EnvStageCapabilities {
			found = e.Value
		}
		if strings.Contains(strings.ToUpper(e.Name), "GOOBERS_CRED_") {
			t.Fatalf("a resolved credential must NEVER be stamped on a pod spec, found %s", e.Name)
		}
	}
	if !strings.Contains(found, "contents:write") || !strings.Contains(found, "issues:write") {
		t.Fatalf("%s = %q, want both declared capability names", EnvStageCapabilities, found)
	}
}

// The skew check compares the TAG to the dispatcher's embedded commit and reads
// no registry (decision 009). That inference holds only if a tag maps to one
// image — and a registry tag is mutable. With the IfNotPresent default, a node
// that cached an earlier push under the same tag keeps serving it, so the skew
// check passes while the pod runs different content.
//
// MEASURED: a Windows runner image rebuilt at the same commit to add the
// daemon's CA was ignored by the node that had the old layers; the stage failed
// x509 twice more after the fix was already in the registry.
func TestPodSpecAlwaysPullsTheStageImage(t *testing.T) {
	pod, err := RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if len(pod.Spec.Containers) == 0 {
		t.Fatal("no containers in the rendered pod")
	}
	if got := pod.Spec.Containers[0].ImagePullPolicy; got != corev1.PullAlways {
		t.Fatalf("ImagePullPolicy = %q, want %q — a mutable tag under IfNotPresent lets a node serve stale content that the skew check cannot see",
			got, corev1.PullAlways)
	}
}

// envDenyRunner is linuxRunner plus the restriction this file now binds to an
// environment stamp rather than to a volume or a security context.
func envDenyRunner() RunnerSpec {
	runner := linuxRunner()
	runner.Restrictions = append(runner.Restrictions, string(runnercap.RestrictionEnvDefaultDeny))
	return runner
}

func testDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer-template"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "stage",
						Image: "ghcr.io/goobers/goobers-base:0123456789abcdef0123456789abcdef01234567",
					}},
				},
			},
		},
	}
}

func podEnv(pod *corev1.Pod) map[string]string {
	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

// #3725: env:default-deny was a fully-wired declaration, validation, placement
// and NetworkPolicy-labeling token with NO enforcement over a pod's
// environment. The dispatcher is where the restriction becomes real, because
// the pod must be TOLD it is under the restriction — a stage that self-reported
// it could turn its own isolation off.
//
// Derived from the RUNNER's enforced set, the same source stampVolumes and
// stampSecurity read: a stage placed on a restricted class gets that class's
// posture whether or not it asked for it, which is the case the issue was filed
// about (cli-stage-probe landed on linux-shell-strict and "worked because the
// restriction is unimplemented").
func TestStagePodStampsEnvDefaultDenyFromTheRunnerClass(t *testing.T) {
	attempt := testAttempt()
	attempt.Env = map[string]string{"DECLARED_STAGE_VAR": "from-the-workflow"}
	attempt.Inputs = map[string]string{"probe": "declared-input"}
	attempt.RunContext = map[string]string{executorRepoNameEnv: "Goobers"}
	cfg := testConfig()
	cfg.EnvPassthrough = []string{"OPERATOR_DECLARED_VAR"}

	pod, err := RenderPod(cfg, attempt, envDenyRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	env := podEnv(pod)
	if env[EnvStageEnvDefaultDeny] != "true" {
		t.Fatalf("%s = %q, want \"true\" on a class enforcing env:default-deny", EnvStageEnvDefaultDeny, env[EnvStageEnvDefaultDeny])
	}
	var allow []string
	if err := json.Unmarshal([]byte(env[EnvStageEnvAllow]), &allow); err != nil {
		t.Fatalf("decode %s (%q): %v", EnvStageEnvAllow, env[EnvStageEnvAllow], err)
	}
	// Everything the DISPATCHER stamped for the stage. In a pod these arrive as
	// ordinary container variables, indistinguishable from the image's own, so
	// procenv's allowlist alone would drop the stage's declared env and inputs.
	for _, want := range []string{"DECLARED_STAGE_VAR", InputEnvVar("probe"), executorRepoNameEnv, "OPERATOR_DECLARED_VAR"} {
		if !slices.Contains(allow, want) {
			t.Fatalf("%s = %v, missing %q", EnvStageEnvAllow, allow, want)
		}
	}
	// NEVER by name. Resolved credentials do not pass through the filter at
	// all — they are minted in-pod and appended after it — so naming them here
	// would be an allowlist entry that looks load-bearing and is not, plus a
	// grant to any image-baked GOOBERS_CRED_*.
	for _, name := range allow {
		if strings.HasPrefix(name, "GOOBERS_CRED_") {
			t.Fatalf("%s must never name a credential variable, found %q (#3725)", EnvStageEnvAllow, name)
		}
	}
}

// Additive only: a class that does not enforce env:default-deny renders exactly
// the pod spec it rendered before this existed.
func TestStagePodOmitsEnvDefaultDenyStampWithoutTheRestriction(t *testing.T) {
	pod, err := RenderPod(testConfig(), testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	env := podEnv(pod)
	for _, name := range []string{EnvStageEnvDefaultDeny, EnvStageEnvAllow} {
		if _, present := env[name]; present {
			t.Fatalf("%s must not be stamped for a class that does not enforce env:default-deny", name)
		}
	}
}

// The template path (DI-9) renders the same stage container, so it must carry
// the same restriction binding — a consumer-authored Deployment template must
// not be a way to run on a restricted class without the restriction.
func TestTemplateStagePodStampsEnvDefaultDeny(t *testing.T) {
	pod, err := RenderFromTemplate(testConfig(), testAttempt(), envDenyRunner(), testDeployment())
	if err != nil {
		t.Fatalf("RenderFromTemplate: %v", err)
	}
	if podEnv(pod)[EnvStageEnvDefaultDeny] != "true" {
		t.Fatalf("the template path must bind env:default-deny too, env = %v", podEnv(pod))
	}
}

// The signal is authority, not identity: a goobers-CLI stage keeps the run
// identity half of the control plane and must STILL not see this.
func TestEnvDefaultDenySignalIsPrivileged(t *testing.T) {
	for _, name := range []string{EnvStageEnvDefaultDeny, EnvStageEnvAllow} {
		if !slices.Contains(DispatcherPrivilegedEnv, name) {
			t.Fatalf("%s must be privileged: a stage that can rewrite it disables its own isolation", name)
		}
		if slices.Contains(DispatcherRunIdentityEnv, name) {
			t.Fatalf("%s is authority, not run identity", name)
		}
	}
}

// The run's operational identity is stamped in stageEnv's own base block, not
// from attempt.Env/Inputs/RunContext — so the first cut of the allowlist did
// not name it, and the in-pod rebuild (which runs BEFORE the CLI/non-CLI strip)
// deleted GOOBERS_RUN_ID/GAGGLE/WORKFLOW/STAGE/ATTEMPT for every stage on a
// declaring class, goobers-CLI stages included.
//
// That is #3725's own failure shape one seam over: providerRunContext() fails
// closed with "GOOBERS_RUN_ID is not set — this subcommand must run as a
// workflow stage", and providers.BranchName cannot compose the run branch, on
// one runner class and not another.
func TestEnvDefaultDenyAllowlistCarriesTheRunIdentityACLIStageKeeps(t *testing.T) {
	pod, err := RenderPod(testConfig(), testAttempt(), envDenyRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	var allow []string
	if err := json.Unmarshal([]byte(podEnv(pod)[EnvStageEnvAllow]), &allow); err != nil {
		t.Fatalf("decode %s: %v", EnvStageEnvAllow, err)
	}
	for _, want := range []string{EnvRunID, EnvGaggle, EnvWorkflow, EnvStage, EnvAttempt} {
		if !slices.Contains(allow, want) {
			t.Fatalf("%s = %v, missing %q — a goobers-CLI stage keeps its run identity by design, and the "+
				"in-pod rebuild runs BEFORE the CLI/non-CLI split, so a name missing here is gone before the "+
				"split can keep it (#3725)", EnvStageEnvAllow, allow, want)
		}
	}
}

// The general form of the bug above, and the durable guard: stageEnvAllowlist
// enumerates its sources BY HAND while stageEnv stamps from ~15 sites, and the
// two lists have no mechanical relationship. Every name the dispatcher stamps
// on a stage container must land in exactly one of three places —
//
//  1. the allowlist          → re-admitted by the in-pod rebuild,
//  2. THIS STAGE's strip set → deliberately removed in the pod,
//  3. procenv's own base     → carried by the floor every stage gets,
//
// — or it is DELETED for a stage on a declaring class and PRESENT for the same
// stage on every other class. That is restriction-conditional breakage
// diagnosed at the far side, which is the shape #3725 itself was filed about.
//
// "THIS STAGE's strip set" is the load-bearing phrase, and the first cut of
// this test got it wrong. The strip is not one set: stageEnvironment() removes
// DispatcherControlEnv from an ordinary stage and only DispatcherPrivilegedEnv
// from a goobers-CLI stage. Checking every stage against the wider set ACCEPTS
// the run-identity gap the test above catches — those names are in
// DispatcherControlEnv, and a CLI stage is nonetheless supposed to receive
// them. So the invariant runs per stage class, against the set that class
// actually loses.
//
// Populated with every Attempt field that reaches stageEnv, so the next
// dispatcher-stamped stage-visible variable fails here rather than in a pod.
func TestEveryStampedStageVarIsAllowlistedOrStrippedOrProcenvBase(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cliStage bool
		stripped []string
	}{
		{"goobers-CLI stage", true, DispatcherPrivilegedEnv},
		{"ordinary stage", false, DispatcherControlEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			attempt := testAttempt()
			attempt.Command = []string{"sh", "-c", "true"}
			attempt.Env = map[string]string{"DECLARED_STAGE_VAR": "from-the-workflow"}
			attempt.Inputs = map[string]string{"probe": "declared-input"}
			attempt.RunContext = map[string]string{
				executorRepoNameEnv:        "Goobers",
				executorRepoOwnerEnv:       "Agent-Clubhouse",
				executorBranchNamespaceEnv: "goobers",
			}
			attempt.CLIStage = tc.cliStage
			attempt.Workspace = "repo"
			attempt.KitDigest = "sha256:0123456789abcdef"
			attempt.CheckoutCapability = "provider:contents:read"
			attempt.WorkspaceDelta = "sha256:fedcba9876543210"
			attempt.Capabilities = []string{"provider:pr:write"}
			cfg := testConfig()
			cfg.EnvPassthrough = []string{"OPERATOR_DECLARED_VAR"}

			pod, err := RenderPod(cfg, attempt, envDenyRunner())
			if err != nil {
				t.Fatalf("RenderPod: %v", err)
			}
			var allow []string
			if err := json.Unmarshal([]byte(podEnv(pod)[EnvStageEnvAllow]), &allow); err != nil {
				t.Fatalf("decode %s: %v", EnvStageEnvAllow, err)
			}

			var orphans []string
			for _, e := range pod.Spec.Containers[0].Env {
				switch {
				case slices.Contains(allow, e.Name):
				case slices.Contains(tc.stripped, e.Name):
				case slices.Contains(procenv.Vars, e.Name):
				default:
					carried := false
					for _, prefix := range procenv.Prefixes {
						if strings.HasPrefix(e.Name, prefix) {
							carried = true
							break
						}
					}
					if !carried {
						orphans = append(orphans, e.Name)
					}
				}
			}
			if len(orphans) > 0 {
				t.Fatalf("the dispatcher stamps %v on a stage container, and env:default-deny neither allowlists "+
					"nor strips them for this stage class: they are silently DELETED on a class enforcing the "+
					"restriction and present everywhere else. Add each to stageEnvAllowlist (stage-visible) or "+
					"to DispatcherPrivilegedEnv (never stage-visible) — #3725", orphans)
			}
		})
	}
}

// A DI-9 consumer template owns the stage container's ambient contract as well
// as its sidecars and volumes. Those names arrive in the pod as ordinary
// container variables, so without the allowlist carrying them the in-pod
// rebuild drops exactly the vars the consumer declared — present on an
// unrestricted class, silently gone on a restricted one.
func TestTemplateDeclaredContainerEnvIsAllowlistedUnderEnvDefaultDeny(t *testing.T) {
	deployment := testDeployment()
	deployment.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "TEMPLATE_DECLARED_VAR", Value: "from-the-consumer-deployment"},
	}

	pod, err := RenderFromTemplate(testConfig(), testAttempt(), envDenyRunner(), deployment)
	if err != nil {
		t.Fatalf("RenderFromTemplate: %v", err)
	}
	env := podEnv(pod)
	if env["TEMPLATE_DECLARED_VAR"] == "" {
		t.Fatalf("the template's own container env must survive rendering, env = %v", env)
	}
	var allow []string
	if err := json.Unmarshal([]byte(env[EnvStageEnvAllow]), &allow); err != nil {
		t.Fatalf("decode %s: %v", EnvStageEnvAllow, err)
	}
	if !slices.Contains(allow, "TEMPLATE_DECLARED_VAR") {
		t.Fatalf("%s = %v omits TEMPLATE_DECLARED_VAR — the in-pod rebuild will drop the var the consumer "+
			"Deployment declared, on a restricted class only (#3725)", EnvStageEnvAllow, allow)
	}
}

// Decision 003's worker-hygiene graft, the stamp half: every dispatcher-created
// stage pod carries the owner label its creator's orphan sweep scopes itself
// to, plus the VERBATIM attempt identity that sweep needs to ADDRESS the
// attempt on the engine.
//
// The labels cannot serve as that address. sanitizeNameSegment lowercases,
// maps every non-alphanumeric rune to '-' and truncates at 63, so it is not
// injective: the identity below has a stage whose label collides with a
// DIFFERENT stage's, and a sweep composing <run>/<stage>/<attempt> out of the
// label would describe some other execution. The answer it would get back is
// "no such workflow" — which this sweep reads as SETTLED and disposes. A lossy
// address on the delete path is the fail-open shape, so the exact strings ride
// as annotations.
func TestRenderPodStampsOwnerAndVerbatimIdentity(t *testing.T) {
	cfg := testConfig()
	cfg.Owner = "goobers-worker-7"
	attempt := testAttempt()
	attempt.Number = 4
	attempt.RunID = "Run.2026_08_22.0001"
	attempt.Stage = "run.unit_tests"
	pod, err := RenderPod(cfg, attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if got := pod.Labels[LabelOwner]; got != "goobers-worker-7" {
		t.Fatalf("%s = %q, want the creating worker's identity", LabelOwner, got)
	}
	if got := pod.Annotations[AnnotationRunID]; got != attempt.RunID {
		t.Fatalf("%s = %q, want the verbatim run id %q", AnnotationRunID, got, attempt.RunID)
	}
	if got := pod.Annotations[AnnotationStage]; got != attempt.Stage {
		t.Fatalf("%s = %q, want the verbatim stage %q", AnnotationStage, got, attempt.Stage)
	}
	if got := pod.Annotations[AnnotationOwningWorkflowID]; got != attempt.OwningWorkflowID {
		t.Fatalf("%s = %q, want the verbatim driver %q", AnnotationOwningWorkflowID, got, attempt.OwningWorkflowID)
	}
	// The driver is not the run, and no rule turns one into the other. The
	// fixture's owning id is the SCHEDULED shape (claimID+"-run" over a RunID
	// the engine rewrote to a hash), so a sweep that composed an address out of
	// the run and stage would describe an execution that does not exist, be
	// told "no such workflow", read that as settled, and delete a live pod.
	if strings.Contains(attempt.OwningWorkflowID, attempt.RunID) {
		t.Fatalf("owning workflow %q contains the run id %q — pick a driver the run id cannot reconstruct, or this test asserts nothing",
			attempt.OwningWorkflowID, attempt.RunID)
	}
	// The labels are lossy for exactly this identity, which is what makes the
	// annotations load-bearing rather than redundant.
	if pod.Labels[LabelRun] == attempt.RunID || pod.Labels[LabelStage] == attempt.Stage {
		t.Fatalf("labels (%q/%q) round-tripped this identity — pick one the sanitizer actually mangles, or this test asserts nothing",
			pod.Labels[LabelRun], pod.Labels[LabelStage])
	}
	// Non-injective, concretely: a different stage sanitizes to the same label.
	if pod.Labels[LabelStage] != sanitizeNameSegment("run-unit-tests", 63) {
		t.Fatalf("stage label %q does not collide with the distinct stage \"run-unit-tests\" — the collision is the reason the address cannot come from the label",
			pod.Labels[LabelStage])
	}
	if got := pod.Labels[LabelAttempt]; got != "4" {
		t.Fatalf("%s = %q, want the attempt ordinal", LabelAttempt, got)
	}
}

// The template path (DI-9) stamps the same ownership and identity: a consumer
// Deployment controls sidecars and volumes, never whether its stage pod can be
// reclaimed by the worker that created it.
func TestRenderFromTemplateStampsOwnerAndVerbatimIdentity(t *testing.T) {
	cfg := testConfig()
	cfg.Owner = "goobers-worker-7"
	template := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "consumer-runner"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "stage", Image: "ghcr.io/consumer/fat:1"}},
				},
			},
		},
	}
	runner := RunnerSpec{
		Name: "consumer", OS: "linux", HostKind: instance.RunnerHostDeployment, Host: "consumer-runner",
	}
	attempt := testAttempt()
	pod, err := RenderFromTemplate(cfg, attempt, runner, template)
	if err != nil {
		t.Fatalf("RenderFromTemplate: %v", err)
	}
	if got := pod.Labels[LabelOwner]; got != "goobers-worker-7" {
		t.Fatalf("%s = %q on the template path", LabelOwner, got)
	}
	if pod.Annotations[AnnotationRunID] != attempt.RunID || pod.Annotations[AnnotationStage] != attempt.Stage ||
		pod.Annotations[AnnotationOwningWorkflowID] != attempt.OwningWorkflowID {
		t.Fatalf("template-path pod carries no verbatim attempt identity: %v", pod.Annotations)
	}
}

// An attempt dispatched with no stated driver stamps NO owning-workflow
// annotation, which makes the pod unaddressable and therefore permanently
// exempt from disposal (TestSweepOrphansLeavesUnaddressablePod). The
// alternative — stamping "" — is an address the resolver would then describe,
// and Temporal's "no such workflow" for the empty id would authorise a delete
// on no evidence at all.
func TestOwningWorkflowAnnotationAbsentWithoutDriver(t *testing.T) {
	attempt := testAttempt()
	attempt.OwningWorkflowID = ""
	pod, err := RenderPod(testConfig(), attempt, linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if got, ok := pod.Annotations[AnnotationOwningWorkflowID]; ok {
		t.Fatalf("%s stamped as %q for an attempt with no driver — an empty address is worse than none", AnnotationOwningWorkflowID, got)
	}
}

// An owner that is not label grammar is sanitized, not rejected and not
// stamped raw — a pod create refused on a hostname is a worker that cannot
// dispatch at all. The sweep's selector goes through the same function, so
// stamp and selector agree by construction.
func TestOwnerLabelSanitized(t *testing.T) {
	cfg := testConfig()
	cfg.Owner = "Worker_A.example.com"
	pod, err := RenderPod(cfg, testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	got := pod.Labels[LabelOwner]
	if got != cfg.ownerLabel() {
		t.Fatalf("stamped owner %q != selector owner %q — a sweep would match nothing", got, cfg.ownerLabel())
	}
	if !labelValue.MatchString(got) {
		t.Fatalf("owner label %q is not valid label grammar", got)
	}
}

// No owner stamps NO label rather than a placeholder: a placeholder is a value
// every other ownerless dispatcher would match too, which is the cross-worker
// disposal the label exists to prevent.
func TestOwnerLabelAbsentWithoutOwner(t *testing.T) {
	cfg := testConfig()
	cfg.Owner = ""
	pod, err := RenderPod(cfg, testAttempt(), linuxRunner())
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if _, ok := pod.Labels[LabelOwner]; ok {
		t.Fatalf("%s stamped without a configured owner: %q", LabelOwner, pod.Labels[LabelOwner])
	}
}

// The identity annotations live in the goobers.dev/* namespace, so the §3
// refuse-to-create already covers them: a workflow cannot pre-set the run id
// its pod claims and thereby point the sweep at somebody else's workflow.
func TestIdentityAnnotationsNonOverridable(t *testing.T) {
	attempt := testAttempt()
	attempt.ExtraAnnotations = map[string]string{AnnotationRunID: "someone-elses-run"}
	if _, err := RenderPod(testConfig(), attempt, linuxRunner()); err == nil {
		t.Fatalf("workflow input set %s and the render accepted it", AnnotationRunID)
	}
	attempt = testAttempt()
	// The owning workflow id is the id a delete is authorised by. A stage that
	// could set it could name a workflow it knows has COMPLETED and have its
	// own live pod swept out from under a competitor's run.
	attempt.ExtraAnnotations = map[string]string{AnnotationOwningWorkflowID: "some-completed-run"}
	if _, err := RenderPod(testConfig(), attempt, linuxRunner()); err == nil {
		t.Fatalf("workflow input set %s and the render accepted it — a stage could point the sweep at a settled workflow", AnnotationOwningWorkflowID)
	}
	attempt = testAttempt()
	attempt.ExtraLabels = map[string]string{LabelOwner: "goobers-worker-99"}
	if _, err := RenderPod(testConfig(), attempt, linuxRunner()); err == nil {
		t.Fatalf("workflow input set %s and the render accepted it — a pod could hide from its owner's sweep", LabelOwner)
	}
}

// labelValue is Kubernetes' label-value grammar.
var labelValue = regexp.MustCompile(`^[a-z0-9A-Z]([-_.a-z0-9A-Z]*[a-z0-9A-Z])?$`)
