package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	corev1 "k8s.io/api/core/v1"

	"github.com/goobers/goobers/internal/launchreceipt"
)

func (d *Dispatcher) recordPreparedLaunch(ctx context.Context, attempt Attempt, pod *corev1.Pod) error {
	if d.cfg.LaunchReceipts == nil {
		return errors.New("dispatcher: launch receipt recorder is required")
	}
	b := attempt.LaunchBinding
	if b == nil || b.RunID != attempt.RunID || b.Stage != attempt.Stage || b.Number != attempt.Number || b.Class != attempt.Class || b.Review != attempt.Review {
		return errors.New("dispatcher: canonical launch binding is missing or mismatched")
	}
	facts, err := preparedPodFacts(pod)
	if err != nil {
		return err
	}
	facts.ExecutionKitDigest = attempt.KitDigest
	r := launchreceipt.Receipt{Version: 1, Binding: *b, Facts: facts}
	if _, err := r.Encode(); err != nil {
		return err
	}
	return d.cfg.LaunchReceipts.Record(ctx, r)
}

func preparedPodFacts(pod *corev1.Pod) (launchreceipt.RemoteFacts, error) {
	f := launchreceipt.RemoteFacts{Source: "control-plane-prepared", Seccomp: "unknown",
		NetworkEnforcement: "unknown", SandboxEnforcement: "unknown", ResolvedModel: "unknown", ResolvedEffort: "unknown"}
	if pod == nil {
		return f, launchreceipt.ErrInvalid
	}
	stage := stageContainerIn(pod.Spec.Containers)
	if stage == nil || stage.Image == "" {
		return f, launchreceipt.ErrInvalid
	}
	f.ImageReferenceDigest = launchreceipt.Digest([]byte(stage.Image))
	// Labels select network policy; this records selection configuration,
	// never asserts that any NetworkPolicy/controller actually enforced it.
	selectors, err := json.Marshal(pod.Labels)
	if err != nil {
		return f, err
	}
	f.SelectorDigest = launchreceipt.Digest(selectors)
	f.HostNetwork, f.HostPID, f.HostIPC = pod.Spec.HostNetwork, pod.Spec.HostPID, pod.Spec.HostIPC
	f.AutomountServiceAccountToken = pod.Spec.AutomountServiceAccountToken
	f.ContainerCount = len(pod.Spec.Containers) + len(pod.Spec.InitContainers)
	if sc := pod.Spec.SecurityContext; sc != nil {
		f.RunAsNonRoot = sc.RunAsNonRoot
		f.Seccomp = preparedSeccomp(sc.SeccompProfile)
	}
	if sc := stage.SecurityContext; sc != nil {
		if sc.RunAsNonRoot != nil {
			f.RunAsNonRoot = sc.RunAsNonRoot
		}
		if sc.SeccompProfile != nil {
			f.Seccomp = preparedSeccomp(sc.SeccompProfile)
		}
		f.Privileged, f.ReadOnlyRootFilesystem, f.AllowPrivilegeEscalation = sc.Privileged, sc.ReadOnlyRootFilesystem, sc.AllowPrivilegeEscalation
		if sc.Capabilities != nil {
			f.DropAllCapabilities = slices.Contains(sc.Capabilities.Drop, corev1.Capability("ALL"))
		}
	}
	for _, mount := range stage.VolumeMounts {
		if !mount.ReadOnly {
			f.WritableMountCount++
		}
	}
	return f, nil
}

func preparedSeccomp(p *corev1.SeccompProfile) string {
	if p == nil {
		return "unknown"
	}
	switch p.Type {
	case corev1.SeccompProfileTypeRuntimeDefault:
		return "runtime-default"
	case corev1.SeccompProfileTypeUnconfined:
		return "unconfined"
	case corev1.SeccompProfileTypeLocalhost:
		return "localhost"
	default:
		return "unknown"
	}
}
