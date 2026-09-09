package dispatcher

import corev1 "k8s.io/api/core/v1"

// observePodPlacement retains facts from the API pod, never the worker host or
// the runner's declared OS. An assigned Pending pod is placement evidence, not
// evidence that a container has started. Unknown observations do not erase a
// previously observed assignment for this supervised pod.
func observePodPlacement(report *Report, pod *corev1.Pod) {
	if pod.Spec.NodeName == "" {
		return
	}
	if report.Node != pod.Spec.NodeName {
		report.Node = pod.Spec.NodeName
		report.OS = ""
	}
	if pod.Spec.OS != nil || pod.Spec.NodeSelector[NodeSelectorOSKey] != "" {
		report.OS = observedPodOS(pod)
	}
}

func observedPodOS(pod *corev1.Pod) string {
	os := pod.Spec.NodeSelector[NodeSelectorOSKey]
	if pod.Spec.OS != nil {
		declared := string(pod.Spec.OS.Name)
		if os != "" && os != declared {
			return "" // conflicting pod constraints do not establish one OS
		}
		os = declared
	}
	if os == "linux" || os == "windows" {
		return os
	}
	return ""
}
