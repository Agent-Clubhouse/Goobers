// Package netpolrender renders the per-runner-class NetworkPolicy reference
// manifests from the runners: inventory — the decision-016 single source of
// the network reference manifests (issue #3568, goobernetes-restrictions.md
// §6/§7, goobernetes-decisions.md 015/016).
//
// One manifest set is emitted per DISTINCT restriction set (runner class,
// restrictions doc §12 render granularity). Every policy SELECTS on
// goobers.dev/runner-class = runnercap.RunnerClassValue(restrictions) — the
// same function the dispatcher stamps stage pods with — so stamp and selector
// agree by construction, never by coincidence (decision 015). No repository
// hand-authors an authoritative parallel copy: a downstream repo that cannot
// import the Go function consumes this rendered output (decision 016).
//
// Composition is additive (delivery decision 004): Kubernetes NetworkPolicy
// has no deny rule and no precedence, so the effective egress of a pod is the
// UNION of every policy selecting it. The gaggle-namespace baseline is
// exactly default-deny-all + allow-dns; every egress grant rendered here
// selects exactly one runner-class label. A generic role-wide grant would
// make every narrower per-class policy a no-op, which is why the reference
// base's former allow-stage-egress policy is gone and this renderer is the
// only producer of stage egress grants.
//
// The renderer produces REFERENCE manifests; it does not apply them and it
// does not probe enforcement. Enforcement honesty (UNVERIFIED-never-PASS) is
// doctor --k8s's job under D12, a separate issue.
package netpolrender

import (
	"fmt"
	"net"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/runnercap"
)

// Allowlist group kinds — the three destination families of the
// k8s-infra-shape §5 stage egress posture, now named per group instead of
// spelled as CHANGE-ME rows.
const (
	// GroupKindProvider is the git/backlog provider destination family
	// (github.com or ADO).
	GroupKindProvider = "provider"
	// GroupKindModel is the model/agent endpoint family the harness uses.
	// The coverage ratchet's reference set is the union of model-kind CIDRs.
	GroupKindModel = "model"
	// GroupKindSandbox is the gaggle's declared sandbox/provisioner targets.
	GroupKindSandbox = "sandbox"
)

// Runner is one pod-hosted runner from the runners: inventory, distilled to
// what the render needs. The caller (cmd/goobers) filters out host:"self"
// entries: a self runner never gets a pod, so it never gets a class policy —
// which is also the zero-declaration invariance: an instance with no runners:
// block resolves to the implicit self entry and renders nothing.
type Runner struct {
	// Name is the inventory entry's name, used in diagnostics and the
	// rendered per-class runner list.
	Name string
	// Restrictions is the entry's declared restriction effect set.
	Restrictions []string
}

// AllowlistGroup is one named CIDR destination group from instance config
// (instance.yaml egress.allowlist). The allowlist CIDR set is
// instance-operator-supplied configuration rendered into the manifests
// (restrictions doc §2.2); the CHANGE-ME documentation CIDRs the reference
// base used to carry are placeholders the render refuses to ship.
type AllowlistGroup struct {
	// Name identifies the group in provenance markers and errors.
	Name string
	// Kind is the destination family: GroupKindProvider, GroupKindModel, or
	// GroupKindSandbox.
	Kind string
	// Source is the upstream document the CIDRs were taken from (e.g.
	// https://api.github.com/meta). Optional; empty means operator-local
	// CIDRs with no upstream to drift from.
	Source string
	// SourceSHA256 is the hex sha256 of the upstream document at the time the
	// CIDRs were transcribed — the provenance marker the drift --check
	// validates against the live document. Required when Source is set.
	SourceSHA256 string
	// CIDRs are the granted destination blocks.
	CIDRs []string
	// Ports are the allowed TCP ports; empty defaults to 443.
	Ports []int
}

// BlobEndpoint locates the blob endpoint every runner class egresses to
// (decisions 010/012, dispatcher §2a): the stage pod's artifact data path,
// crossing namespaces from the gaggle namespace into goobers-system.
type BlobEndpoint struct {
	// Namespace is the endpoint's namespace, pinned in the SAME peer element
	// as the pod selector (the AND form — see composeCrossNamespacePeer).
	Namespace string
	// PodLabels select exactly the endpoint's pod within that namespace.
	PodLabels map[string]string
	// Port is the endpoint's container port.
	Port int
}

// DefaultBlobEndpoint is the v1 daemon-fronted blob endpoint (dispatcher
// §2a): the goobers-api pod in goobers-system on its container port.
func DefaultBlobEndpoint() BlobEndpoint {
	return BlobEndpoint{
		Namespace: "goobers-system",
		PodLabels: map[string]string{"app.kubernetes.io/name": "goobers-api"},
		Port:      8080,
	}
}

// Input is everything one render consumes.
type Input struct {
	// Runners are the pod-hosted inventory entries (self entries already
	// filtered out by the caller).
	Runners []Runner
	// Allowlist is the instance-operator-supplied destination set.
	Allowlist []AllowlistGroup
	// Blob is the blob endpoint; zero value means DefaultBlobEndpoint.
	Blob BlobEndpoint
}

// Class is one distinct restriction set — one runner class — with everything
// derived from it.
type Class struct {
	// Value is the goobers.dev/runner-class label value,
	// runnercap.RunnerClassValue over Restrictions.
	Value string
	// Restrictions is the canonical (deduplicated, sorted) restriction set.
	Restrictions []string
	// Preimage is the AnnotationRunnerClassRestrictions value — the
	// human-readable decode of Value, emitted from the SAME canonical input
	// the value was derived from.
	Preimage string
	// Runners are the inventory entries that resolve to this class, sorted.
	Runners []string
	// NetworkNone is true when the set contains network:none — the class
	// gets only DNS and the blob data path.
	NetworkNone bool
}

// File is one rendered manifest file.
type File struct {
	// Name is the file's basename, e.g. "netpol-netallow.yaml".
	Name string
	// Content is the full YAML, header comments included.
	Content []byte
}

// Result is a completed render.
type Result struct {
	// Classes are the distinct runner classes, sorted by Value.
	Classes []Class
	// Files are the per-class manifest files plus the kustomization, in
	// deterministic order.
	Files []File
	// Policies are the rendered policy objects by class value — what the
	// tests parse instead of grepping (dispatcher §2a: the composed peer is
	// verified by parsing it, not reading it).
	Policies map[string]*networkingv1.NetworkPolicy
}

// documentationCIDRs are the RFC 5737 / RFC 3849 documentation ranges — the
// exact CHANGE-ME placeholder values the reference base used to carry. A
// config that still holds one has not been filled in, and the render fails
// rather than emitting a stub that looks deployable (restrictions doc line
// 258: "render refuses CHANGE-ME placeholders").
var documentationCIDRs = []string{"192.0.2.0/24", "198.51.100.0/24", "203.0.113.0/24", "2001:db8::/32"}

// Classes derives the distinct runner classes from the pod-hosted inventory,
// sorted by class value. Two runners with the same restriction SET (order and
// duplicates ignored) are one class.
func Classes(runners []Runner) []Class {
	byValue := make(map[string]*Class)
	for _, runner := range runners {
		canonical := runnercap.CanonicalRestrictions(runner.Restrictions)
		value := runnercap.RunnerClassValue(canonical)
		class, ok := byValue[value]
		if !ok {
			class = &Class{
				Value:        value,
				Restrictions: canonical,
				Preimage:     runnercap.RunnerClassPreimage(canonical),
				NetworkNone:  contains(canonical, string(runnercap.RestrictionNetworkNone)),
			}
			byValue[value] = class
		}
		class.Runners = append(class.Runners, runner.Name)
	}
	out := make([]Class, 0, len(byValue))
	for _, class := range byValue {
		sort.Strings(class.Runners)
		out = append(out, *class)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Value < out[j].Value })
	return out
}

// Render emits the per-class NetworkPolicy manifest set. An empty inventory
// renders an empty result and no error — the zero-declaration invariance.
func Render(input Input) (*Result, error) {
	classes := Classes(input.Runners)
	result := &Result{Classes: classes, Policies: make(map[string]*networkingv1.NetworkPolicy, len(classes))}
	if len(classes) == 0 {
		return result, nil
	}
	if err := validateAllowlist(classes, input.Allowlist); err != nil {
		return nil, err
	}
	blob := input.Blob
	if blob.Namespace == "" && len(blob.PodLabels) == 0 && blob.Port == 0 {
		blob = DefaultBlobEndpoint()
	}

	for _, class := range classes {
		policy := classPolicy(class, input.Allowlist, blob)
		content, err := marshalPolicy(class, input.Allowlist, policy)
		if err != nil {
			return nil, err
		}
		result.Policies[class.Value] = policy
		result.Files = append(result.Files, File{Name: classFileName(class.Value), Content: content})
	}
	result.Files = append(result.Files, kustomizationFile(classes))
	return result, nil
}

// validateAllowlist enforces the fail-don't-stub contract: a class that needs
// a destination set (anything but network:none) with no configured allowlist,
// or any group still holding a documentation-placeholder CIDR, refuses the
// whole render.
func validateAllowlist(classes []Class, groups []AllowlistGroup) error {
	for _, group := range groups {
		for _, cidr := range group.CIDRs {
			if err := refusePlaceholderCIDR(cidr); err != nil {
				return fmt.Errorf("egress.allowlist group %q: %w", group.Name, err)
			}
		}
	}
	if len(groups) > 0 {
		return nil
	}
	var needy []string
	for _, class := range classes {
		if !class.NetworkNone {
			needy = append(needy, fmt.Sprintf("%s (runners %s)", class.Value, strings.Join(class.Runners, ", ")))
		}
	}
	if len(needy) > 0 {
		return fmt.Errorf("no egress.allowlist is configured, but %d runner class(es) need a destination set: %s — "+
			"declare egress.allowlist in instance.yaml (the CIDR set is operator-supplied configuration; "+
			"the render does not emit placeholder stubs)", len(needy), strings.Join(needy, "; "))
	}
	return nil
}

// refusePlaceholderCIDR rejects the literal CHANGE-ME marker and the
// documentation ranges the reference base used as CHANGE-ME values.
func refusePlaceholderCIDR(cidr string) error {
	if strings.Contains(cidr, "CHANGE-ME") {
		return fmt.Errorf("CIDR %q is an unfilled CHANGE-ME placeholder; fill in the real destination range", cidr)
	}
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return fmt.Errorf("CIDR %q does not parse: %w", cidr, err)
	}
	if !ip.Equal(ipNet.IP) {
		return fmt.Errorf("CIDR %q has host bits set (its network is %s) — spell the network address exactly, "+
			"a normalized-away host part is how a grant silently widens", cidr, ipNet)
	}
	for _, doc := range documentationCIDRs {
		_, docNet, _ := net.ParseCIDR(doc)
		if docNet.Contains(ipNet.IP) {
			return fmt.Errorf("CIDR %q is inside the documentation range %s — these are the CHANGE-ME placeholder "+
				"values from the reference base, not routable destinations; fill in the real range", cidr, doc)
		}
	}
	return nil
}

// classPolicy builds one class's NetworkPolicy object. Every class —
// network:none INCLUDED — carries the DNS row and the blob-endpoint row
// (decision 012: the blob path is the class's own data path, not a grant to
// withhold; without it a restricted stage hangs at materialize). Classes
// without network:none additionally carry every configured allowlist group.
func classPolicy(class Class, groups []AllowlistGroup, blob BlobEndpoint) *networkingv1.NetworkPolicy {
	egress := []networkingv1.NetworkPolicyEgressRule{dnsEgressRule(), blobEgressRule(blob)}
	if !class.NetworkNone {
		for _, group := range groups {
			egress = append(egress, groupEgressRule(group))
		}
	}
	return &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "stage-egress-" + class.Value,
			Annotations: map[string]string{
				runnercap.AnnotationRunnerClassRestrictions: class.Preimage,
				annotationRunnerClassRunners:                strings.Join(class.Runners, ","),
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			// Selecting BOTH the role marker and the class label narrows the
			// grant to stage pods of exactly this class; the dispatcher
			// stamps both (decision 015 round-trip: stamp == selector).
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				runnercap.LabelRole:        runnercap.RoleStage,
				runnercap.LabelRunnerClass: class.Value,
			}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}
}

// annotationRunnerClassRunners lists the inventory runner names that resolve
// to a policy's class — diagnostics only, never selected on.
const annotationRunnerClassRunners = "goobers.dev/runner-class-runners"

// composeCrossNamespacePeer is THE security-critical composition of this
// package (dispatcher §2a, decision 012 — the case that has burned this
// project three times): a cross-namespace grant combines namespaceSelector
// AND podSelector in a SINGLE to/from element. The namespaceSelector reaches
// the other namespace, the podSelector pins the one pod. The same two
// selectors as SEPARATE list elements would OR — every pod in the namespace,
// plus the pod in every namespace: the egress-proxy bypass. A podSelector
// alone would select the policy's OWN namespace, match nothing, and grant
// nothing — the stage hangs at materialize, indistinguishable from a missing
// row. Both failure shapes differ from this one by two characters of YAML
// indentation, which is why the tests PARSE the composed peer rather than
// grep for it.
func composeCrossNamespacePeer(namespace string, podLabels map[string]string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{
		NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			"kubernetes.io/metadata.name": namespace,
		}},
		PodSelector: &metav1.LabelSelector{MatchLabels: podLabels},
	}
}

func dnsEgressRule() networkingv1.NetworkPolicyEgressRule {
	udp, tcp := protocolUDP, protocolTCP
	port := intstr.FromInt32(53)
	return networkingv1.NetworkPolicyEgressRule{
		To: []networkingv1.NetworkPolicyPeer{
			composeCrossNamespacePeer("kube-system", map[string]string{"k8s-app": "kube-dns"}),
		},
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &udp, Port: &port},
			{Protocol: &tcp, Port: &port},
		},
	}
}

func blobEgressRule(blob BlobEndpoint) networkingv1.NetworkPolicyEgressRule {
	tcp := protocolTCP
	port := intstr.FromInt32(int32(blob.Port))
	return networkingv1.NetworkPolicyEgressRule{
		To:    []networkingv1.NetworkPolicyPeer{composeCrossNamespacePeer(blob.Namespace, blob.PodLabels)},
		Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: &port}},
	}
}

func groupEgressRule(group AllowlistGroup) networkingv1.NetworkPolicyEgressRule {
	tcp := protocolTCP
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(group.CIDRs))
	for _, cidr := range group.CIDRs {
		peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	ports := group.Ports
	if len(ports) == 0 {
		ports = []int{443}
	}
	policyPorts := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for _, p := range ports {
		port := intstr.FromInt32(int32(p))
		policyPorts = append(policyPorts, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: &port})
	}
	return networkingv1.NetworkPolicyEgressRule{To: peers, Ports: policyPorts}
}

// marshalPolicy emits one policy file: a header naming the render contract,
// the class preimage, and every allowlist group's provenance marker, then the
// policy YAML.
func marshalPolicy(class Class, groups []AllowlistGroup, policy *networkingv1.NetworkPolicy) ([]byte, error) {
	body, err := yaml.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("marshal class %s policy: %w", class.Value, err)
	}
	var header strings.Builder
	header.WriteString("# Rendered by `goobers netpol-render` — do NOT hand-edit (decision 016:\n")
	header.WriteString("# the rendered output is the only authoritative copy; downstream repos\n")
	header.WriteString("# consume it, never a hand-maintained parallel copy).\n")
	fmt.Fprintf(&header, "# runner class %s = restriction set [%s]\n", class.Value, class.Preimage)
	fmt.Fprintf(&header, "# runners: %s\n", strings.Join(class.Runners, ", "))
	if !class.NetworkNone {
		for _, group := range groups {
			if group.Source != "" {
				fmt.Fprintf(&header, "# provenance: %s (%s) %s sha256:%s\n", group.Name, group.Kind, group.Source, group.SourceSHA256)
			} else {
				fmt.Fprintf(&header, "# provenance: %s (%s) operator-local, no upstream source declared\n", group.Name, group.Kind)
			}
		}
	}
	return append([]byte(header.String()), body...), nil
}

func classFileName(value string) string {
	return "netpol-" + value + ".yaml"
}

func kustomizationFile(classes []Class) File {
	var b strings.Builder
	b.WriteString("# Rendered by `goobers netpol-render` — do NOT hand-edit (decision 016).\n")
	b.WriteString("# Compose into each gaggle namespace alongside the gaggle-namespace base\n")
	b.WriteString("# (default-deny-all + allow-dns); these per-class grants are the ONLY stage\n")
	b.WriteString("# egress grants (delivery decision 004: grants are per-class only).\n")
	b.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n")
	for _, class := range classes {
		fmt.Fprintf(&b, "  - %s\n", classFileName(class.Value))
	}
	return File{Name: "kustomization.yaml", Content: []byte(b.String())}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

var (
	protocolTCP = corev1.ProtocolTCP
	protocolUDP = corev1.ProtocolUDP
)
