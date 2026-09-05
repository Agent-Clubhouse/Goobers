package k8spreflight

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func checkAPIServerIPBlockDrift(ctx context.Context, client kubernetes.Interface, opts Options) Result {
	result := Result{
		ID:       "apiserver-ipblock-drift",
		Title:    "NetworkPolicy API-server ipBlock matches the live endpoint",
		Citation: "§5",
		Severity: SeverityRequired,
	}
	if opts.APIServerEndpoint == "" {
		result.Severity = SeverityOptional
		result.Status = StatusWarn
		result.Detail = "cannot determine the live API-server endpoint"
		result.Hint = "rerun with a kubeconfig that provides the API-server endpoint"
		return result
	}
	endpoint, err := url.Parse(opts.APIServerEndpoint)
	if err != nil || endpoint.Hostname() == "" {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("invalid API-server endpoint %q", opts.APIServerEndpoint)
		result.Hint = "verify the kubeconfig cluster server URL"
		return result
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", endpoint.Hostname())
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("cannot resolve API-server endpoint %q: %v", endpoint.Hostname(), err)
		result.Hint = "verify the kubeconfig cluster server URL and DNS reachability"
		return result
	}
	policies, err := client.NetworkingV1().NetworkPolicies("").List(ctx, metav1.ListOptions{})
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("unable to list NetworkPolicies: %v", err)
		result.Hint = "grant list on networkpolicies across namespaces to the preflighting identity"
		return result
	}
	checked := 0
	var mismatches []string
	for _, policy := range policies.Items {
		checkPeers := func(peers []networkingv1.NetworkPolicyPeer) {
			for _, peer := range peers {
				if peer.IPBlock == nil {
					continue
				}
				checked++
				network := peer.IPBlock.CIDR
				ip, cidr, parseErr := net.ParseCIDR(network)
				if parseErr != nil {
					mismatches = append(mismatches, policy.Namespace+"/"+policy.Name+"="+network)
					continue
				}
				ones, bits := cidr.Mask.Size()
				if ip.String() != cidr.IP.String() || ones != bits {
					mismatches = append(mismatches, policy.Namespace+"/"+policy.Name+"="+network)
					continue
				}
				matches := false
				for _, liveIP := range ips {
					if cidr.Contains(liveIP) {
						matches = true
						break
					}
				}
				if !matches {
					mismatches = append(mismatches, policy.Namespace+"/"+policy.Name+"="+network)
				}
			}
		}
		for _, ingress := range policy.Spec.Ingress {
			checkPeers(ingress.From)
		}
		for _, egress := range policy.Spec.Egress {
			checkPeers(egress.To)
		}
	}
	if checked == 0 {
		result.Status = StatusFail
		result.Detail = "incident: API calls can time out as RBAC or auth symptoms when no API-server ipBlock entries are inspected (checked 0)"
		result.Hint = "render a NetworkPolicy egress ipBlock for the live API-server endpoint"
		return result
	}
	if len(mismatches) > 0 {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("incident: API calls can time out as RBAC or auth symptoms when API-server ipBlock drifts; checked %d ipBlock entries, mismatches: %s", checked, strings.Join(mismatches, ", "))
		result.Hint = "replace stale API-server ipBlock CIDRs with the live endpoint address"
		return result
	}
	result.Status = StatusPass
	result.Detail = fmt.Sprintf("checked %d API-server ipBlock entries; all match the live endpoint", checked)
	return result
}

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
		Title:    "runner-class requests fit the node pool (incident I-55 / O-11)",
		Citation: "§7",
		Severity: SeverityRequired,
	}
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("unable to list nodes: %v", err)
		result.Hint = "grant list on nodes to the preflighting identity so runner-class capacity is verified against the pool"
		return result
	}
	if len(nodes.Items) == 0 {
		result.Status = StatusWarn
		result.Detail = "checked 0 node(s); no pool capacity was available to verify"
		result.Hint = "a non-empty cluster is required before runner-class capacity can be checked"
		return result
	}

	maxCPU := int64(0)
	maxMemory := int64(0)
	for _, node := range nodes.Items {
		if cpu := node.Status.Allocatable.Cpu().MilliValue(); cpu > maxCPU {
			maxCPU = cpu
		}
		if mem := node.Status.Allocatable.Memory().Value(); mem > maxMemory {
			maxMemory = mem
		}
	}

	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("unable to list pods: %v", err)
		result.Hint = "grant list on pods across namespaces so runner-class requests can be checked against node allocatable capacity"
		return result
	}

	classRequests := map[string]struct{ cpu, memory int64 }{}
	for _, pod := range pods.Items {
		class, ok := pod.Labels["goobers.dev/runner-class"]
		if !ok || class == "" {
			continue
		}
		entry := classRequests[class]
		for _, c := range pod.Spec.InitContainers {
			entry.cpu += c.Resources.Requests.Cpu().MilliValue()
			entry.memory += c.Resources.Requests.Memory().Value()
		}
		for _, c := range pod.Spec.Containers {
			entry.cpu += c.Resources.Requests.Cpu().MilliValue()
			entry.memory += c.Resources.Requests.Memory().Value()
		}
		classRequests[class] = entry
	}
	if len(classRequests) == 0 {
		result.Status = StatusWarn
		result.Detail = "checked 0 runner class(es); no pods carry a goobers.dev/runner-class label"
		result.Hint = "a runner class must be declared and populated before its request ceiling can be evaluated against node allocatable capacity"
		return result
	}

	var offenders []string
	for class, req := range classRequests {
		if req.cpu > maxCPU || req.memory > maxMemory {
			offenders = append(offenders, fmt.Sprintf("%s requests %dm CPU / %d bytes vs node-pool ceiling %dm CPU / %d bytes", class, req.cpu, req.memory, maxCPU, maxMemory))
		}
	}
	if len(offenders) > 0 {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("checked %d runner class(es) across %d node(s); %s", len(classRequests), len(nodes.Items), strings.Join(offenders, "; "))
		result.Hint = "lower the runner-class request ceiling or expand the node pool so every declared class fits within the allocatable capacity a node can actually provide"
		return result
	}

	result.Status = StatusPass
	result.Detail = fmt.Sprintf("checked %d runner class(es) across %d node(s); every class fits within the largest allocatable node (%dm CPU / %d bytes)", len(classRequests), len(nodes.Items), maxCPU, maxMemory)
	return result
}

func checkPodHealth(ctx context.Context, client kubernetes.Interface, _ Options) Result {
	result := Result{
		ID:       "pod-health",
		Title:    "per-container pod health (incident: crash-looping sidecar mistaken as healthy)",
		Citation: "§7",
		Severity: SeverityRequired,
	}
	pods, err := client.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("unable to list pods: %v", err)
		result.Hint = "grant list on pods so every container's readiness and restart count can be examined"
		return result
	}

	checked := 0
	var unhealthy []string
	for _, pod := range pods.Items {
		statuses := make(map[string]corev1.ContainerStatus, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
		for _, status := range pod.Status.InitContainerStatuses {
			statuses[status.Name] = status
		}
		for _, status := range pod.Status.ContainerStatuses {
			statuses[status.Name] = status
		}
		for _, container := range append(pod.Spec.InitContainers, pod.Spec.Containers...) {
			checked++
			status, ok := statuses[container.Name]
			if !ok {
				unhealthy = append(unhealthy, fmt.Sprintf("%s/%s %s status missing from pod status", pod.Namespace, pod.Name, container.Name))
				continue
			}
			if status.RestartCount > 0 || !status.Ready || status.State.Waiting != nil || (status.State.Terminated != nil && status.State.Terminated.ExitCode != 0) {
				reasonParts := []string{fmt.Sprintf("%s/%s %s", pod.Namespace, pod.Name, container.Name)}
				if status.RestartCount > 0 {
					reasonParts = append(reasonParts, fmt.Sprintf("restarts=%d", status.RestartCount))
				}
				if !status.Ready {
					reasonParts = append(reasonParts, "not ready")
				}
				if status.State.Waiting != nil {
					reasonParts = append(reasonParts, "waiting: "+status.State.Waiting.Reason)
				}
				if status.State.Terminated != nil {
					reasonParts = append(reasonParts, fmt.Sprintf("terminated exit=%d", status.State.Terminated.ExitCode))
				}
				unhealthy = append(unhealthy, strings.Join(reasonParts, "; "))
			}
		}
	}
	if checked == 0 {
		result.Status = StatusWarn
		result.Detail = "checked 0 container(s); no pod inventory was present to inspect"
		result.Hint = "a live cluster must expose pod inventory before a pod-health preflight can assess container readiness"
		return result
	}
	if len(unhealthy) > 0 {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("checked %d container(s) across %d pod(s); unhealthy: %s", checked, len(pods.Items), strings.Join(unhealthy, "; "))
		result.Hint = "inspect every container by name from status.containerStatuses[name], not containerStatuses[0]; a log-healthy sidecar with restarts is not a healthy pod"
		return result
	}
	result.Status = StatusPass
	result.Detail = fmt.Sprintf("checked %d container(s) across %d pod(s); every container is ready and restart-free", checked, len(pods.Items))
	return result
}

func checkOTLPSignalSet(ctx context.Context, _ kubernetes.Interface, opts Options) Result {
	result := Result{
		ID:       "otlp-signal-set",
		Title:    "OTLP signal set accepts traces, metrics, and logs (incident #4261)",
		Citation: "§4",
		Severity: SeverityRequired,
	}
	if opts.OTLPEndpoint == "" {
		result.Severity = SeverityOptional
		result.Status = StatusWarn
		result.Detail = "skipped — no OTLP collector endpoint configured"
		result.Hint = "set the collector endpoint in config or environment so the preflight can verify the collector accepts traces, metrics, and logs"
		return result
	}
	baseURL := strings.TrimRight(opts.OTLPEndpoint, "/")
	signals := []string{"traces", "metrics", "logs"}
	var rejected []string
	for _, signal := range signals {
		endpoint := baseURL + "/v1/" + signal
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			rejected = append(rejected, fmt.Sprintf("%s query not constructed: %v", signal, err))
			continue
		}
		response, err := opts.httpClient().Do(request)
		if err != nil {
			rejected = append(rejected, fmt.Sprintf("%s query never ran: %v", signal, err))
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			_ = response.Body.Close()
			continue
		}
		_ = response.Body.Close()
		rejected = append(rejected, fmt.Sprintf("%s returned HTTP %d", signal, response.StatusCode))
	}
	if len(rejected) > 0 {
		result.Status = StatusFail
		result.Detail = fmt.Sprintf("checked %d signal route(s); %s", len(signals), strings.Join(rejected, "; "))
		result.Hint = "the configured collector must accept traces, metrics, and logs; a request that never ran is different from a request that returned no data"
		return result
	}
	result.Status = StatusPass
	result.Detail = fmt.Sprintf("checked %d signal route(s); traces, metrics, and logs all accepted by the configured OTLP collector", len(signals))
	return result
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
