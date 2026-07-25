package k8spreflight

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
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
		Title:    "NetworkPolicy API served (deny-first defaults enforceable)",
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
			result.Status = StatusPass
			result.Detail = "networking.k8s.io/v1 networkpolicies served — enforcement still depends on the CNI"
			result.Hint = "a CNI without NetworkPolicy support ignores policies silently; verify deny-first takes effect with a probe pod"
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
		Title:    "ReadWriteMany-capable StorageClass for journal & artifacts",
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
	var names []string
	for _, class := range classes.Items {
		if rwxCapable(class.Provisioner) {
			result.Status = StatusPass
			result.Detail = fmt.Sprintf("class %q (provisioner %s) supports ReadWriteMany — capability inferred from the provisioner, a PVC bind probe is a follow-up", class.Name, class.Provisioner)
			return result
		}
		names = append(names, class.Name)
	}
	result.Status = StatusFail
	if len(names) == 0 {
		result.Detail = "the cluster has no StorageClasses"
	} else {
		result.Detail = fmt.Sprintf("no RWX-capable class among: %s", strings.Join(names, ", "))
	}
	result.Hint = "provision an RWX-capable class (Azure Files/Blob CSI, NFS, CephFS, …) for the shared journal volume (§4)"
	return result
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
