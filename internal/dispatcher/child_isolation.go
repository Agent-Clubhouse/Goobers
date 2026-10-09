package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/goobers/goobers/internal/instance"
)

// ErrChildIsolation refuses an execution or custody acknowledgement whose
// process/container boundary cannot be proved. Ordinary dispatch is unchanged.
var ErrChildIsolation = errors.New("dispatcher: isolated child pod custody is unconfirmed")

var childContractDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

const childCustodyFinalizer = "goobers.dev/isolated-workspace-custody"
const childTerminationGrace = 120
const childStopTimeout = 150 * time.Second

// childPodAPI keeps creation identity and conditional deletion atomic at the
// Kubernetes API. A name-only API cannot safely acknowledge child writers.
type childPodAPI interface {
	CreatePodWithIdentity(context.Context, *corev1.Pod) (*corev1.Pod, error)
	DeletePodWithIdentity(context.Context, string, string, types.UID) error
	FinalizePodWithIdentity(context.Context, string, string, types.UID) error
}

func validateChildRunner(attempt Attempt, runner RunnerSpec) error {
	if attempt.ChildExecutionDigest == "" {
		return nil
	}
	if !childContractDigest.MatchString(attempt.ChildExecutionDigest) || runner.HostKind != instance.RunnerHostImage || runner.OS != "linux" || attempt.CheckoutCapability != "" || attempt.CLIStage {
		return fmt.Errorf("%w: child execution requires a Linux image runner without checkout credentials or operational CLI authority", ErrChildIsolation)
	}
	return nil
}

func hardenChildPod(pod *corev1.Pod, attempt Attempt) (*corev1.Pod, error) {
	if pod == nil || len(pod.Spec.Containers) != 1 {
		return nil, ErrChildIsolation
	}
	spec := &pod.Spec
	pod.Finalizers = []string{childCustodyFinalizer}
	spec.TerminationGracePeriodSeconds = ptr.To[int64](childTerminationGrace)
	container := &spec.Containers[0]
	spec.HostPID, spec.HostIPC, spec.HostNetwork = false, false, false
	spec.ShareProcessNamespace = ptr.To(false)
	spec.AutomountServiceAccountToken = ptr.To(false)
	spec.EnableServiceLinks = ptr.To(false)
	spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](1000), RunAsGroup: ptr.To[int64](1000), FSGroup: ptr.To[int64](1000), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	container.SecurityContext = &corev1.SecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](1000), RunAsGroup: ptr.To[int64](1000), Privileged: ptr.To(false), AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}
	// No shared caches, image HOME, projected credentials, or host files enter
	// the child. All writable volumes die with this single-use pod.
	spec.Volumes = []corev1.Volume{}
	container.VolumeMounts = []corev1.VolumeMount{}
	for _, volume := range []struct{ name, path string }{{"workspace", LinuxWorkspacePath}, {"home", LinuxHomePath}, {"tmp", LinuxTmpPath}} {
		spec.Volumes = append(spec.Volumes, corev1.Volume{Name: volume.name, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: volume.name, MountPath: volume.path})
	}
	container.Env = slices.DeleteFunc(container.Env, func(v corev1.EnvVar) bool {
		return slices.Contains([]string{"HOME", "TMPDIR", "GOMODCACHE", "GOCACHE", EnvChildExecutionDigest, EnvStageEnvAllow, EnvStageEnvDefaultDeny}, v.Name)
	})
	allow, _ := json.Marshal(stageEnvAllowlist(Config{}, attempt, nil))
	container.Env = append(container.Env, corev1.EnvVar{Name: EnvStageEnvAllow, Value: string(allow)}, corev1.EnvVar{Name: EnvStageEnvDefaultDeny, Value: "true"})
	container.Env = append(container.Env, corev1.EnvVar{Name: "HOME", Value: LinuxHomePath}, corev1.EnvVar{Name: "TMPDIR", Value: LinuxTmpPath}, corev1.EnvVar{Name: "GOMODCACHE", Value: LinuxHomePath + "/go/pkg/mod"}, corev1.EnvVar{Name: "GOCACHE", Value: LinuxHomePath + "/.cache/go-build"}, corev1.EnvVar{Name: EnvChildExecutionDigest, Value: attempt.ChildExecutionDigest})
	return pod, validateChildPod(pod, attempt)
}

func validateChildPod(pod *corev1.Pod, attempt Attempt) error {
	if pod == nil || !childNamespaceIsolated(pod.Spec) || !slices.Contains(pod.Finalizers, childCustodyFinalizer) {
		return ErrChildIsolation
	}
	c := pod.Spec.Containers[0]
	if !childContainerIsolated(c) || !childVolumesIsolated(pod.Spec, c) {
		return ErrChildIsolation
	}
	for _, v := range c.Env {
		if v.Name == EnvChildExecutionDigest && v.Value == attempt.ChildExecutionDigest && v.ValueFrom == nil {
			return nil
		}
	}
	return ErrChildIsolation
}

func childNamespaceIsolated(s corev1.PodSpec) bool {
	if s.OS == nil || s.OS.Name != corev1.Linux || s.HostPID || s.HostIPC || s.HostNetwork || ptr.Deref(s.ShareProcessNamespace, true) || ptr.Deref(s.AutomountServiceAccountToken, true) || s.RestartPolicy != corev1.RestartPolicyNever {
		return false
	}
	if len(s.Containers) != 1 || len(s.InitContainers) != 0 || len(s.EphemeralContainers) != 0 || s.SecurityContext == nil || s.SecurityContext.SeccompProfile == nil {
		return false
	}
	return s.SecurityContext.SeccompProfile.Type == corev1.SeccompProfileTypeRuntimeDefault
}

func childContainerIsolated(c corev1.Container) bool {
	s := c.SecurityContext
	if c.Name != StageContainerName || s == nil || !ptr.Deref(s.RunAsNonRoot, false) || ptr.Deref(s.RunAsUser, int64(0)) <= 0 || ptr.Deref(s.Privileged, true) || ptr.Deref(s.AllowPrivilegeEscalation, true) || !ptr.Deref(s.ReadOnlyRootFilesystem, false) {
		return false
	}
	if s.Capabilities == nil || len(s.Capabilities.Add) != 0 || !slices.Contains(s.Capabilities.Drop, corev1.Capability("ALL")) || len(c.EnvFrom) != 0 || len(c.VolumeDevices) != 0 {
		return false
	}
	return s.SeccompProfile == nil || s.SeccompProfile.Type == corev1.SeccompProfileTypeRuntimeDefault
}

func childVolumesIsolated(s corev1.PodSpec, c corev1.Container) bool {
	paths := map[string]string{"workspace": LinuxWorkspacePath, "home": LinuxHomePath, "tmp": LinuxTmpPath}
	if len(s.Volumes) != 3 || len(c.VolumeMounts) != 3 {
		return false
	}
	for _, v := range s.Volumes {
		if v.EmptyDir == nil || paths[v.Name] == "" || !reflect.DeepEqual(v.VolumeSource, corev1.VolumeSource{EmptyDir: v.EmptyDir}) {
			return false
		}
	}
	for _, m := range c.VolumeMounts {
		if m.SubPath != "" || m.SubPathExpr != "" || m.MountPropagation != nil || paths[m.Name] != m.MountPath {
			return false
		}
	}
	return true
}

func (d *Dispatcher) createAttemptPod(ctx context.Context, pod *corev1.Pod, attempt Attempt, report *Report) error {
	if attempt.ChildExecutionDigest == "" {
		return d.pods.CreatePod(ctx, pod)
	}
	api, ok := d.pods.(childPodAPI)
	if !ok {
		return fmt.Errorf("%w: Kubernetes adapter lacks atomic pod identity", ErrChildIsolation)
	}
	report.ChildCreateAttempted = true
	created, err := api.CreatePodWithIdentity(ctx, pod)
	if err != nil {
		return err
	}
	if created == nil || created.UID == "" || created.Name != pod.Name || created.Namespace != pod.Namespace {
		return ErrChildIsolation
	}
	pod.UID = created.UID
	report.ChildPodUID = string(created.UID)
	return validateChildPod(created, attempt)
}

func observeChildWriters(attempt Attempt, pod *corev1.Pod, report *Report) error {
	if attempt.ChildExecutionDigest == "" {
		return nil
	}
	if pod == nil || report.ChildPodUID == "" || string(pod.UID) != report.ChildPodUID {
		return ErrChildIsolation
	}
	if err := validateChildPod(pod, attempt); err != nil {
		return err
	}
	if len(pod.Status.ContainerStatuses) == 1 {
		status := pod.Status.ContainerStatuses[0]
		report.WorkspaceWritersStopped = status.Name == StageContainerName && status.ContainerID != "" && status.State.Terminated != nil
	}
	return nil
}

func (d *Dispatcher) disposeChildPod(ctx context.Context, created *corev1.Pod, attempt Attempt) error {
	api, ok := d.pods.(childPodAPI)
	if !ok || created.UID == "" {
		return ErrChildIsolation
	}
	observed, err := d.pods.GetPod(ctx, created.Namespace, created.Name)
	if err != nil {
		return err
	}
	report := Report{ChildPodUID: string(created.UID)}
	if err := observeChildWriters(attempt, observed, &report); err != nil || !report.WorkspaceWritersStopped {
		return ErrChildIsolation
	}
	confirmed, err := d.gate.Confirmed(ctx, attempt)
	if err != nil || !confirmed {
		return ErrRecoveryUnconfirmed
	}
	if err := api.DeletePodWithIdentity(ctx, created.Namespace, created.Name, created.UID); err != nil {
		return err
	}
	return api.FinalizePodWithIdentity(ctx, created.Namespace, created.Name, created.UID)
}

// A graceful delete signals PID1 while the finalizer preserves the API object
// until the worker observes exact termination and durable surrender. Absence
// alone never acknowledges writers. No force deletion is used.
func (d *Dispatcher) superviseAttempt(ctx context.Context, attempt Attempt, pod *corev1.Pod, report *Report) (corev1.PodPhase, error) {
	phase, err := d.supervise(ctx, attempt, pod.Namespace, pod.Name, report)
	if attempt.ChildExecutionDigest == "" || ctx.Err() == nil || report.WorkspaceWritersStopped {
		return phase, err
	}
	api, ok := d.pods.(childPodAPI)
	if !ok || pod.UID == "" {
		return phase, errors.Join(err, ErrChildIsolation)
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), childStopTimeout)
	defer cancel()
	if stopErr := api.DeletePodWithIdentity(cleanup, pod.Namespace, pod.Name, pod.UID); stopErr != nil {
		return phase, errors.Join(err, stopErr)
	}
	return d.supervise(cleanup, attempt, pod.Namespace, pod.Name, report)
}

func selectChildAwareRunner(attempt Attempt, eligible []RunnerSpec) (RunnerSpec, error) {
	selected, err := SelectRunner(attempt, eligible)
	if err != nil {
		return RunnerSpec{}, err
	}
	return selected, validateChildRunner(attempt, selected)
}

func (d *Dispatcher) prepareAttemptKit(ctx context.Context, attempt *Attempt) error {
	if !attempt.Agentic {
		return nil
	}
	if attempt.ChildExecutionDigest != "" {
		if !childContractDigest.MatchString(attempt.KitDigest) {
			return fmt.Errorf("%w: child requires an exact prepublished agentic kit", ErrChildIsolation)
		}
		return nil
	}
	if d.cfg.KitWriter == nil {
		return fmt.Errorf("dispatcher: agentic stage %s of run %s requires a kit writer; none is configured", attempt.Stage, attempt.RunID)
	}
	digest, err := d.cfg.KitWriter.WriteKit(ctx, *attempt)
	if err != nil {
		return fmt.Errorf("dispatcher: publish agentic kit for run %s stage %s attempt %d: %w", attempt.RunID, attempt.Stage, attempt.Number, err)
	}
	if digest == "" {
		return fmt.Errorf("dispatcher: agentic kit writer returned no digest for run %s stage %s", attempt.RunID, attempt.Stage)
	}
	attempt.KitDigest = digest
	return nil
}

func childSettlementContext(ctx context.Context, attempt Attempt) (context.Context, context.CancelFunc) {
	if attempt.ChildExecutionDigest == "" {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), DefaultDisposalTimeout)
}

// A distinct signed token domain identifies generated custody at the daemon,
// even if lineage is missing. It must never fall through to shared blob storage.
type childTokenMinter interface {
	MintChildPod(string, time.Duration) (string, error)
}

func (d *Dispatcher) mintAttemptToken(attempt *Attempt) error {
	var token string
	var err error
	if attempt.ChildExecutionDigest != "" {
		minter, ok := d.cfg.TokenMinter.(childTokenMinter)
		if !ok || attempt.PodToken != "" {
			return fmt.Errorf("%w: generated custody requires a freshly signed child token", ErrChildIsolation)
		}
		token, err = minter.MintChildPod(attempt.RunID, 0)
		if err == nil && token == "" {
			return ErrChildIsolation
		}
	} else if d.cfg.TokenMinter != nil && attempt.PodToken == "" {
		token, err = d.cfg.TokenMinter.Mint(attempt.RunID, 0)
	} else {
		return nil
	}
	if err != nil {
		return fmt.Errorf("dispatcher: mint pod token for run %s stage %s attempt %d: %w", attempt.RunID, attempt.Stage, attempt.Number, err)
	}
	attempt.PodToken = token
	return nil
}
