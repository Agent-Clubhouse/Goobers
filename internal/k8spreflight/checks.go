package k8spreflight

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// minSupportedMinor is the oldest 1.x minor the preflight considers current.
// It tracks the compatibility window of the pinned client-go (±3 minors) —
// revisit on client-go bumps, and keep it a warn: version support is the
// customer's upgrade cadence (§1), not a hard install blocker.
const minSupportedMinor = 29

// controlPlaneNamespace is where the goobers-system install lands (§3) and
// therefore where the namespaced install permissions are probed.
const controlPlaneNamespace = "goobers-system"

const temporalNamespace = "goobers-temporal"

func checkClusterVersion(_ context.Context, client kubernetes.Interface, _ Options) Result {
	result := Result{
		ID:       "cluster-version",
		Title:    "cluster reachable, supported Kubernetes version",
		Citation: "§1",
		Severity: SeverityRequired,
	}
	info, err := client.Discovery().ServerVersion()
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("cannot reach the cluster: %v", err)
		result.Hint = "check kubeconfig/context and connectivity — every other check depends on this"
		return result
	}
	minor, parseErr := strconv.Atoi(strings.TrimRight(info.Minor, "+"))
	switch {
	case parseErr != nil:
		result.Status = StatusWarn
		result.Detail = fmt.Sprintf("server reports %s — cannot parse the minor version %q", info.GitVersion, info.Minor)
		result.Hint = "verify the server runs a supported Kubernetes release"
	case info.Major != "1" || minor < minSupportedMinor:
		result.Status = StatusWarn
		result.Detail = fmt.Sprintf("server runs %s, older than the 1.%d floor this preflight expects", info.GitVersion, minSupportedMinor)
		result.Hint = "upgrade the cluster (§1: upgrades are customer-operated) or verify support explicitly"
	default:
		result.Status = StatusPass
		result.Detail = fmt.Sprintf("Kubernetes %s", info.GitVersion)
	}
	return result
}

func checkNetworkPolicySupport(_ context.Context, client kubernetes.Interface, _ Options) Result {
	result := Result{
		ID:       "networkpolicy-api",
		Title:    "NetworkPolicy API served (enforcement unverified — needs a negative control)",
		Citation: "§5",
		Severity: SeverityRequired,
	}
	resources, err := client.Discovery().ServerResourcesForGroupVersion("networking.k8s.io/v1")
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("networking.k8s.io/v1 not served: %v", err)
		result.Hint = "the per-gaggle deny-first policies (§5) cannot be expressed without the NetworkPolicy API"
		return result
	}
	for _, resource := range resources.APIResources {
		if resource.Name == "networkpolicies" {
			// API-discovery only: a served API is a correlate of enforcement,
			// not proof of it — a CNI can serve networking.k8s.io/v1 and still
			// drop policies on the floor silently. This check is read-only
			// (doctor --k8s never mutates the cluster), so it cannot fire the
			// denied-attempt probe that would actually prove enforcement; that
			// stays a warn, not a pass, until such a probe runs.
			result.Status = StatusWarn
			result.Detail = "networking.k8s.io/v1 networkpolicies served, but enforcement is unverified by this read-only check"
			result.Hint = "a served API is only a correlate — a CNI can serve NetworkPolicy and still ignore it silently; enforcement can only be proven by a denied attempt (an in-cluster negative control, e.g. the Goobernetes S9 probe), not by this check"
			return result
		}
	}
	result.Status = StatusFail
	result.Detail = "networking.k8s.io/v1 is served but has no networkpolicies resource"
	result.Hint = "install a CNI with NetworkPolicy support so the §5 deny-first defaults are enforceable"
	return result
}

// accessProbe is one SelfSubjectAccessReview the RBAC checks issue, with the
// label a denial is reported under.
type accessProbe struct {
	label string
	attrs authorizationv1.ResourceAttributes
}

// runAccessProbes issues each probe and folds the outcomes into result:
// an SSAR transport error fails closed, denials fail with the denied labels,
// and full allowance passes.
func runAccessProbes(ctx context.Context, client kubernetes.Interface, probes []accessProbe, result Result) Result {
	var denied []string
	for _, probe := range probes {
		review := &authorizationv1.SelfSubjectAccessReview{
			Spec: authorizationv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &probe.attrs,
			},
		}
		response, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			result.Status = StatusFail
			result.Detail = fmt.Sprintf("unable to verify %q: %v", probe.label, err)
			result.Hint = "the identity in the kubeconfig cannot issue SelfSubjectAccessReviews — rerun as the installing identity (fail-closed, never a silent pass)"
			return result
		}
		if !response.Status.Allowed {
			denied = append(denied, probe.label)
		}
	}
	if len(denied) > 0 {
		result.Status = StatusFail
		result.Detail = "denied: " + strings.Join(denied, ", ")
		result.Hint = "the install needs cluster-admin-equivalent scope (§1); grant the missing permissions to the installing identity"
		return result
	}
	result.Status = StatusPass
	result.Detail = fmt.Sprintf("all %d install permissions granted to the current identity", len(probes))
	return result
}

func checkInstallRBAC(ctx context.Context, client kubernetes.Interface, _ Options) Result {
	result := Result{
		ID:       "rbac-install",
		Title:    "permissions to install the goobers-system control plane",
		Citation: "§1/§3",
		Severity: SeverityRequired,
	}
	probes := []accessProbe{
		{"create customresourcedefinitions", authorizationv1.ResourceAttributes{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions", Verb: "create"}},
		{"create namespaces", authorizationv1.ResourceAttributes{Resource: "namespaces", Verb: "create"}},
		{"create clusterroles", authorizationv1.ResourceAttributes{Group: "rbac.authorization.k8s.io", Resource: "clusterroles", Verb: "create"}},
		{"create deployments in " + controlPlaneNamespace, authorizationv1.ResourceAttributes{Group: "apps", Resource: "deployments", Verb: "create", Namespace: controlPlaneNamespace}},
		{"create serviceaccounts in " + controlPlaneNamespace, authorizationv1.ResourceAttributes{Resource: "serviceaccounts", Verb: "create", Namespace: controlPlaneNamespace}},
		{"create roles in " + controlPlaneNamespace, authorizationv1.ResourceAttributes{Group: "rbac.authorization.k8s.io", Resource: "roles", Verb: "create", Namespace: controlPlaneNamespace}},
	}
	return runAccessProbes(ctx, client, probes, result)
}

func checkGaggleRBAC(ctx context.Context, client kubernetes.Interface, _ Options) Result {
	result := Result{
		ID:       "rbac-gaggle",
		Title:    "permissions to stamp per-gaggle namespaces",
		Citation: "§3/§5",
		Severity: SeverityRequired,
	}
	// Namespace: "" asks for the permission across all namespaces — gaggle
	// namespaces do not exist yet at preflight time.
	probes := []accessProbe{
		{"create networkpolicies (all namespaces)", authorizationv1.ResourceAttributes{Group: "networking.k8s.io", Resource: "networkpolicies", Verb: "create"}},
		{"create serviceaccounts (all namespaces)", authorizationv1.ResourceAttributes{Resource: "serviceaccounts", Verb: "create"}},
	}
	return runAccessProbes(ctx, client, probes, result)
}

// rwxCapable reports whether a StorageClass provisioner is known to support
// ReadWriteMany volumes (§4: RWX-capable class or blob-backed CSI mount).
func rwxCapable(provisioner string) bool {
	known := []string{
		"file.csi.azure.com",           // reference substrate: Azure Files (§4)
		"blob.csi.azure.com",           // reference substrate: blob-backed CSI (§4)
		"efs.csi.aws.com",              // vendor-neutral equivalents
		"filestore.csi.storage.gke.io", //
		"cephfs.csi.ceph.com",          //
		"smb.csi.k8s.io",               //
	}
	if slices.Contains(known, provisioner) {
		return true
	}
	// NFS-flavored provisioners (in-tree, csi-driver-nfs, third-party) are
	// RWX by construction, as is the in-tree kubernetes.io/azure-file.
	return strings.Contains(provisioner, "nfs") || strings.Contains(provisioner, "azure-file")
}

func checkStorage(ctx context.Context, client kubernetes.Interface, _ Options) Result {
	result := Result{
		ID:       "storage-rwx",
		Title:    "StorageClass safe for the instance root's file coordination",
		Citation: "§4",
		Severity: SeverityRequired,
	}
	classes, err := client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("unable to list StorageClasses: %v", err)
		result.Hint = "grant list on storageclasses to the preflighting identity (fail-closed, never a silent pass)"
		return result
	}
	if len(classes.Items) == 0 {
		result.Status = StatusFail
		result.Detail = "the cluster has no StorageClasses"
		result.Hint = "provision a StorageClass for the instance root; RWO mounted by a single node is the recommended safe default (§4)"
		return result
	}
	for _, class := range classes.Items {
		if rwxCapable(class.Provisioner) {
			result.Status = StatusWarn
			result.Detail = fmt.Sprintf("class %q (provisioner %s) may support ReadWriteMany, but provisioner-name inference cannot verify cross-client POSIX flock or SQLite WAL safety", class.Name, class.Provisioner)
			result.Hint = "do not place an instance root containing lock files or SQLite databases on RWX/network storage; use RWO storage with a single node until storage roles are split or a cross-client safety probe is available"
			return result
		}
	}
	result.Status = StatusPass
	result.Detail = "no RWX-capable class found; the cluster's StorageClasses default to ReadWriteOnce, the recommended safe topology for the instance root (§4)"
	result.Hint = "mount the instance root by a single node — do not scale the daemon deployment beyond one replica until lock-bearing state is split from projected journal/artifact storage"
	return result
}

func checkMixedOSPlacement(ctx context.Context, client kubernetes.Interface, _ Options) Result {
	result := Result{
		ID:       "mixed-os-placement",
		Title:    "Linux workloads cannot schedule onto Windows nodes",
		Citation: "§7",
		Severity: SeverityRequired,
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("unable to list nodes: %v", err)
		result.Hint = "grant list on nodes to the preflighting identity (fail-closed, never a silent pass)"
		return result
	}

	var windowsNodes []string
	var untaintedWindowsNodes []string
	for _, node := range nodes.Items {
		if node.Labels[corev1.LabelOSStable] != "windows" {
			continue
		}
		windowsNodes = append(windowsNodes, node.Name)
		if !hasWindowsNoScheduleTaint(node.Spec.Taints) {
			untaintedWindowsNodes = append(untaintedWindowsNodes, node.Name)
		}
	}
	if len(windowsNodes) == 0 {
		result.Status = StatusPass
		result.Detail = "no Windows nodes found; mixed-OS scheduling cannot occur"
		return result
	}

	var unpinnedWorkloads []string
	for _, namespace := range []string{controlPlaneNamespace, temporalNamespace} {
		deployments, listErr := client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
		if listErr != nil {
			result.Status = StatusFail
			result.Detail = fmt.Sprintf("unable to list Deployments in %s: %v", namespace, listErr)
			result.Hint = "grant list on deployments in shipped workload namespaces so Linux workload placement can be verified"
			return result
		}
		for _, deployment := range deployments.Items {
			if deployment.Spec.Template.Spec.NodeSelector[corev1.LabelOSStable] != "linux" {
				unpinnedWorkloads = append(unpinnedWorkloads, namespace+"/Deployment/"+deployment.Name)
			}
		}

		statefulSets, listErr := client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
		if listErr != nil {
			result.Status = StatusFail
			result.Detail = fmt.Sprintf("unable to list StatefulSets in %s: %v", namespace, listErr)
			result.Hint = "grant list on statefulsets in shipped workload namespaces so Linux workload placement can be verified"
			return result
		}
		for _, statefulSet := range statefulSets.Items {
			if statefulSet.Spec.Template.Spec.NodeSelector[corev1.LabelOSStable] != "linux" {
				unpinnedWorkloads = append(unpinnedWorkloads, namespace+"/StatefulSet/"+statefulSet.Name)
			}
		}

		jobs, listErr := client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
		if listErr != nil {
			result.Status = StatusFail
			result.Detail = fmt.Sprintf("unable to list Jobs in %s: %v", namespace, listErr)
			result.Hint = "grant list on jobs in shipped workload namespaces so Linux workload placement can be verified"
			return result
		}
		for _, job := range jobs.Items {
			if job.Spec.Template.Spec.NodeSelector[corev1.LabelOSStable] != "linux" {
				unpinnedWorkloads = append(unpinnedWorkloads, namespace+"/Job/"+job.Name)
			}
		}
	}
	if len(untaintedWindowsNodes) > 0 || len(unpinnedWorkloads) > 0 {
		var problems []string
		if len(untaintedWindowsNodes) > 0 {
			problems = append(problems, "Windows nodes missing kubernetes.io/os=windows:NoSchedule taint: "+strings.Join(untaintedWindowsNodes, ", "))
		}
		if len(unpinnedWorkloads) > 0 {
			problems = append(problems, "shipped workloads missing kubernetes.io/os=linux nodeSelector: "+strings.Join(unpinnedWorkloads, ", "))
		}
		result.Status = StatusFail
		result.Detail = strings.Join(problems, "; ")
		result.Hint = "taint every Windows node and pin every Linux workload; an unpinned pod can attach and destroy a Linux filesystem volume on Windows"
		return result
	}

	result.Status = StatusPass
	result.Detail = fmt.Sprintf("%d Windows node(s) tainted NoSchedule; all shipped workloads pinned to Linux", len(windowsNodes))
	return result
}

func checkRunnerClassCapacity(ctx context.Context, client kubernetes.Interface, _ Options) Result {
	result := Result{
		ID:       "runner-class-capacity",
		Title:    "runner-class requests fit node allocatable capacity",
		Citation: "§7",
		Severity: SeverityRequired,
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("unable to list nodes: %v", err)
		result.Hint = "grant list on nodes so the preflight can compare runner-class requests to allocatable capacity"
		return result
	}
	allocatableCPU := make(map[string]int64, len(nodes.Items))
	for _, node := range nodes.Items {
		allocatableCPU[node.Name] = node.Status.Allocatable.Cpu().MilliValue()
	}

	pods, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("unable to list pods: %v", err)
		result.Hint = "grant list on pods so the preflight can compare runner-class requests to node allocatable capacity"
		return result
	}

	var violations []string
	checked := 0
	for _, pod := range pods.Items {
		if pod.Labels["goobers.dev/runner-class"] == "" || pod.Spec.NodeName == "" {
			continue
		}
		checked++
		allocatable, ok := allocatableCPU[pod.Spec.NodeName]
		if !ok {
			violations = append(violations, fmt.Sprintf("pod %s/%s on unknown node %q", pod.Namespace, pod.Name, pod.Spec.NodeName))
			continue
		}
		requested := int64(0)
		for _, container := range pod.Spec.Containers {
			requested += container.Resources.Requests.Cpu().MilliValue()
		}
		if requested > allocatable {
			violations = append(violations, fmt.Sprintf("pod %s/%s (%s) requests %dm CPU on node %s but allocatable is %dm", pod.Namespace, pod.Name, pod.Labels["goobers.dev/runner-class"], requested, pod.Spec.NodeName, allocatable))
		}
	}
	if checked == 0 {
		result.Status = StatusWarn
		result.Detail = "checked 0 runner-class pods — no runner-class pod requests were inspected"
		result.Hint = "a cluster with no covered runner-class pods is not evidence of safety; inspect the node pool or runner-class inventory"
		return result
	}
	if len(violations) > 0 {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("checked %d runner-class pod(s); %d request/allocatable mismatch(es): %s", checked, len(violations), strings.Join(violations, "; "))
		result.Hint = "reduce runner-class CPU requests or provision a node pool whose allocatable CPU covers the class ceiling"
		return result
	}
	result.Status = StatusPass
	result.Detail = fmt.Sprintf("checked %d runner-class pod(s); all requests fit their node allocatable CPU", checked)
	return result
}

func checkPodHealth(ctx context.Context, client kubernetes.Interface, _ Options) Result {
	result := Result{
		ID:       "pod-container-health",
		Title:    "per-container pod health (crash-looping sidecars are not healthy)",
		Citation: "§5",
		Severity: SeverityRequired,
	}
	pods, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("unable to list pods: %v", err)
		result.Hint = "grant list on pods so the preflight can inspect each container by name"
		return result
	}

	var unhealthy []string
	checked := 0
	for _, pod := range pods.Items {
		for _, c := range pod.Spec.Containers {
			checked++
			status, ok := containerStatusByName(pod.Status.ContainerStatuses, c.Name)
			if !ok {
				unhealthy = append(unhealthy, fmt.Sprintf("%s/%s:%s has no status yet", pod.Namespace, pod.Name, c.Name))
				continue
			}
			if status.State.Waiting != nil {
				switch status.State.Waiting.Reason {
				case "CrashLoopBackOff", "CreateContainerConfigError", "ImagePullBackOff", "ErrImagePull", "RunContainerError":
					unhealthy = append(unhealthy, fmt.Sprintf("%s/%s:%s waiting=%s restart=%d", pod.Namespace, pod.Name, c.Name, status.State.Waiting.Reason, status.RestartCount))
				}
			}
			if status.State.Terminated != nil && status.State.Terminated.ExitCode != 0 {
				unhealthy = append(unhealthy, fmt.Sprintf("%s/%s:%s terminated with exit code %d", pod.Namespace, pod.Name, c.Name, status.State.Terminated.ExitCode))
			}
			if status.State.Running == nil && status.State.Waiting == nil && status.State.Terminated == nil {
				unhealthy = append(unhealthy, fmt.Sprintf("%s/%s:%s has no running, waiting, or terminated state", pod.Namespace, pod.Name, c.Name))
			}
		}
	}
	if checked == 0 {
		result.Status = StatusWarn
		result.Detail = "checked 0 containers — no pod containers were inspected"
		result.Hint = "a cluster with no pod containers to inspect is not evidence of health; make sure the workload inventory is present"
		return result
	}
	if len(unhealthy) > 0 {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("checked %d container(s); unhealthy container(s): %s", checked, strings.Join(unhealthy, "; "))
		result.Hint = "fix crash-looping sidecars and any container that never reaches a healthy running state before trusting the pod as healthy"
		return result
	}
	result.Status = StatusPass
	result.Detail = fmt.Sprintf("checked %d container(s) across %d pod(s); all containers are healthy", checked, len(pods.Items))
	return result
}

func containerStatusByName(statuses []corev1.ContainerStatus, name string) (corev1.ContainerStatus, bool) {
	for _, status := range statuses {
		if status.Name == name {
			return status, true
		}
	}
	return corev1.ContainerStatus{}, false
}

func checkOTLPSignalSet(ctx context.Context, _ kubernetes.Interface, opts Options) Result {
	result := Result{
		ID:       "otlp-signal-set",
		Title:    "OTLP collector accepts traces, metrics, and logs",
		Citation: "§4",
		Severity: SeverityOptional,
	}
	if opts.OTLPEndpoint == "" {
		result.Status = StatusWarn
		result.Detail = "skipped — no OTLP collector endpoint configured"
		result.Hint = "configure the telemetry collector endpoint so the preflight can verify traces, metrics, and logs pipes are all present"
		return result
	}
	result.Severity = SeverityRequired
	base := strings.TrimRight(opts.OTLPEndpoint, "/")
	client := opts.httpClient()
	var failed []string
	var warnings []string
	passed := 0
	checked := 0
	for _, signal := range []string{"traces", "metrics", "logs"} {
		checked++
		url := base + "/v1/" + signal
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte(`{"resourceSpans":[]}`)))
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: invalid URL: %v", signal, err))
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: query never ran: %v", signal, err))
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 400 {
			passed++
			if len(body) == 0 {
				warnings = append(warnings, fmt.Sprintf("%s: query returned no payload (accepted but empty)", signal))
			}
			continue
		}
		failed = append(failed, fmt.Sprintf("%s: collector replied HTTP %d%s", signal, resp.StatusCode, maybeOTLPReason(body)))
	}
	if checked == 0 {
		result.Status = StatusFail
		result.Detail = "checked 0 signal paths — the collector was not queried"
		result.Hint = "a configured OTLP collector must answer all three signal endpoints"
		return result
	}
	if len(failed) > 0 {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("checked %d signal(s); %d accepted, %d rejected: %s", checked, passed, len(failed), strings.Join(failed, "; "))
		result.Hint = "fix the collector pipeline so it accepts traces, metrics, and logs; a metrics-only or logs-only collector is not a complete signal set"
		return result
	}
	if len(warnings) > 0 {
		result.Status = StatusWarn
		result.Detail = fmt.Sprintf("checked %d signal(s); %d accepted, 0 rejected: %s", checked, passed, strings.Join(warnings, "; "))
		result.Hint = "the collector accepted the signal paths but returned no payloads; verify the collector pipeline and that it is actually receiving telemetry"
		return result
	}
	result.Status = StatusPass
	result.Detail = fmt.Sprintf("checked %d signal(s); traces, metrics, and logs all reached the collector", checked)
	return result
}

func maybeOTLPReason(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	return ": " + trimmed
}

func hasWindowsNoScheduleTaint(taints []corev1.Taint) bool {
	for _, taint := range taints {
		if taint.Key == corev1.LabelOSStable && taint.Value == "windows" && taint.Effect == corev1.TaintEffectNoSchedule {
			return true
		}
	}
	return false
}

func checkOIDCIssuer(ctx context.Context, _ kubernetes.Interface, opts Options) Result {
	result := Result{
		ID:       "oidc-issuer",
		Title:    "OIDC issuer discovery document reachable",
		Citation: "§1/§3",
	}
	if opts.OIDCIssuer == "" {
		result.Severity = SeverityOptional
		result.Status = StatusWarn
		result.Detail = "skipped — no issuer configured"
		result.Hint = "§1 assumes an OIDC issuer for portal/API auth; rerun with --oidc-issuer <url>"
		return result
	}
	result.Severity = SeverityRequired
	discoveryURL := strings.TrimSuffix(opts.OIDCIssuer, "/") + "/.well-known/openid-configuration"
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("invalid issuer URL: %v", err)
		return result
	}
	response, err := opts.httpClient().Do(request)
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("discovery document not reachable from this host: %v", err)
		result.Hint = "verify the issuer URL and this host's egress; in-cluster reachability is probed separately (follow-up)"
		return result
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("discovery document returned HTTP %d", response.StatusCode)
		result.Hint = "the issuer must serve /.well-known/openid-configuration"
		return result
	}
	var document struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil || document.Issuer == "" {
		result.Status = StatusFail
		result.Detail = "discovery document is not a valid OIDC configuration (no issuer field)"
		return result
	}
	result.Status = StatusPass
	result.Detail = fmt.Sprintf("issuer %q discoverable from this host", document.Issuer)
	return result
}

func checkRegistry(ctx context.Context, _ kubernetes.Interface, opts Options) Result {
	// Host-side sanity only (severity optional): node-level pulls use the
	// nodes' own egress and credentials, which this host cannot exercise —
	// the definitive check is a labeled probe pod (documented follow-up).
	result := Result{
		ID:       "registry",
		Title:    "container registry reachable (host-side sanity)",
		Citation: "§1",
		Severity: SeverityOptional,
	}
	if opts.Registry == "" {
		result.Status = StatusWarn
		result.Detail = "skipped — no registry configured"
		result.Hint = "§1 requires a registry the cluster can pull from; rerun with --registry <host>, and verify node pulls with a probe pod"
		return result
	}
	base := opts.Registry
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/v2/", nil)
	if err != nil {
		result.Status = StatusWarn
		result.Detail = fmt.Sprintf("invalid registry %q: %v", opts.Registry, err)
		return result
	}
	response, err := opts.httpClient().Do(request)
	if err != nil {
		result.Status = StatusWarn
		result.Detail = fmt.Sprintf("not reachable from this host: %v", err)
		result.Hint = "cluster nodes may still reach it via their own egress — verify with a probe pod pull"
		return result
	}
	defer func() { _ = response.Body.Close() }()
	// Any HTTP response proves reachability; 401/403 is the normal
	// unauthenticated answer from a private registry.
	result.Status = StatusPass
	result.Detail = fmt.Sprintf("reachable from this host (HTTP %d) — node pull auth is not exercised here", response.StatusCode)
	return result
}

func checkEgress(ctx context.Context, _ kubernetes.Interface, opts Options) Result {
	result := Result{
		ID:       "egress",
		Title:    "required outbound targets reachable",
		Citation: "§1/§5",
	}
	if len(opts.Egress) == 0 {
		result.Severity = SeverityOptional
		result.Status = StatusWarn
		result.Detail = "skipped — no egress targets configured"
		result.Hint = "list the git/backlog provider, model endpoint, and sandbox targets: --egress github.com:443,…"
		return result
	}
	result.Severity = SeverityRequired
	dial := opts.dialContext()
	var unreachable []string
	for _, target := range opts.Egress {
		if _, _, err := net.SplitHostPort(target); err != nil {
			unreachable = append(unreachable, fmt.Sprintf("%s (want host:port)", target))
			continue
		}
		dialCtx, cancel := context.WithTimeout(ctx, opts.timeout())
		conn, err := dial(dialCtx, "tcp", target)
		cancel()
		if err != nil {
			unreachable = append(unreachable, target)
			continue
		}
		_ = conn.Close()
	}
	if len(unreachable) > 0 {
		result.Status = StatusFail
		result.Detail = "unreachable from this host: " + strings.Join(unreachable, ", ")
		result.Hint = "stage pods need exactly these targets under deny-first policies (§5); in-cluster egress may still differ — probe-pod verification is a follow-up"
		return result
	}
	result.Status = StatusPass
	result.Detail = fmt.Sprintf("%d target(s) reachable from this host — in-cluster egress is shaped by the §5 policies", len(opts.Egress))
	return result
}
