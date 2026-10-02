package k8spreflight

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
)

// checkPodSecurityAdmission measures current admission of representative image-
// based stage pods. It never creates a persisted pod, changes namespace policy,
// or claims that acceptance under baseline proves acceptance under restricted.
func checkPodSecurityAdmission(ctx context.Context, client kubernetes.Interface, opts Options) Result {
	result := Result{ID: "pod-security-admission", Title: "stage pod admission (server dry-run)", Citation: "#5284", Severity: SeverityOptional, Status: StatusPass}
	namespaces := slices.Clone(opts.PSANamespaces)
	if len(namespaces) == 0 {
		probeCtx, cancel := context.WithTimeout(ctx, opts.timeout())
		found, err := client.CoreV1().Namespaces().List(probeCtx, metav1.ListOptions{LabelSelector: "goobers.dev/gaggle"})
		cancel()
		if err != nil {
			result.Status, result.Detail = StatusWarn, "unchecked: cannot discover gaggle namespaces: "+err.Error()
			return result
		}
		for _, ns := range found.Items {
			namespaces = append(namespaces, ns.Name)
		}
	}
	slices.Sort(namespaces)
	namespaces = slices.Compact(namespaces)
	if len(namespaces) == 0 {
		result.Status, result.Detail = StatusWarn, "unchecked: no gaggle namespaces; supply --psa-namespaces"
		return result
	}
	var verdicts []string
	for _, namespace := range namespaces {
		probeCtx, cancel := context.WithTimeout(ctx, opts.timeout())
		ns, err := client.CoreV1().Namespaces().Get(probeCtx, namespace, metav1.GetOptions{})
		cancel()
		if err != nil {
			result.Status = StatusWarn
			verdicts = append(verdicts, namespace+": unchecked: "+err.Error())
			continue
		}
		policy := ns.Labels["pod-security.kubernetes.io/enforce"]
		if policy == "" {
			policy = "cluster default"
		}
		policyVersion := ns.Labels["pod-security.kubernetes.io/enforce-version"]
		if policyVersion != "" {
			policy += ":" + policyVersion
		}
		for _, os := range []string{"linux", "windows"} {
			pod, err := admissionProbe(namespace, os, opts.PSAServiceAccount)
			if err == nil {
				probeCtx, cancel := context.WithTimeout(ctx, opts.timeout())
				_, err = client.CoreV1().Pods(namespace).Create(probeCtx, pod, metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
				cancel()
			}
			verdict := "accepted"
			if err != nil {
				result.Status = StatusWarn
				verdict = "not admitted: " + err.Error()
			}
			verdicts = append(verdicts, fmt.Sprintf("%s (enforce=%s), %s: %s", namespace, policy, os, verdict))
		}
	}
	result.Detail = strings.Join(verdicts, "; ")
	result.Hint = "Informational: tests representative dispatcher image pods under current policy; custom templates, accounts and admission webhooks may differ. Acceptance under baseline does not establish restricted compatibility."
	return result
}

func admissionProbe(namespace, os, account string) (*corev1.Pod, error) {
	cfg := dispatcher.Config{GaggleNamespaces: map[string]string{"doctor": namespace}, GaggleServiceAccounts: map[string]string{"doctor": account}}
	attempt := dispatcher.Attempt{RunID: "doctor-admission", Gaggle: "doctor", Workflow: "doctor", Stage: "admission", Number: 1}
	runner := dispatcher.RunnerSpec{Name: "doctor", OS: os, HostKind: instance.RunnerHostImage, Host: "registry.k8s.io/pause:3.10"}
	pod, err := dispatcher.RenderPod(cfg, attempt, runner)
	if err != nil {
		return nil, err
	}
	pod.Name = ""
	pod.GenerateName = "goobers-doctor-admission-"
	return pod, nil
}
