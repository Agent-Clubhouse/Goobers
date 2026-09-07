package main

import (
	"net/netip"
	"os"
	"slices"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

func TestDeployReferenceControlPlaneHasNamespaceWideDenyFloor(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/reference/goobers-system/networkpolicies.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var floor *networkingv1.NetworkPolicy
	for _, doc := range splitYAMLDocs(raw) {
		var policy networkingv1.NetworkPolicy
		if err := yaml.UnmarshalStrict(doc, &policy); err != nil {
			t.Fatal(err)
		}
		if policy.Name == "default-deny-all" {
			floor = &policy
		}
	}
	if floor == nil {
		t.Fatal("goobers-system has no default-deny floor; a worker allow policy does not isolate other pods")
	}
	if floor.Namespace != "goobers-system" || len(floor.Spec.PodSelector.MatchLabels) != 0 || len(floor.Spec.PodSelector.MatchExpressions) != 0 {
		t.Fatalf("floor does not select every control-plane pod: %+v", floor)
	}
	if !slices.Contains(floor.Spec.PolicyTypes, networkingv1.PolicyTypeIngress) || !slices.Contains(floor.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) || len(floor.Spec.Ingress) != 0 || len(floor.Spec.Egress) != 0 {
		t.Fatalf("floor does not deny both directions: %+v", floor.Spec)
	}
	kustomization, err := os.ReadFile("../../deploy/reference/goobers-system/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var base struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(kustomization, &base); err != nil || !slices.Contains(base.Resources, "networkpolicies.yaml") {
		t.Fatalf("floor is not wired into the shipped base: %v", err)
	}
}

func TestDeployReferenceControlPlanePolicyUnionHasNoInternetBypass(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/reference/goobers-system/networkpolicies.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var policies []networkingv1.NetworkPolicy
	for _, doc := range splitYAMLDocs(raw) {
		var policy networkingv1.NetworkPolicy
		if err := yaml.UnmarshalStrict(doc, &policy); err != nil {
			t.Fatal(err)
		}
		policies = append(policies, policy)
	}
	for _, component := range []string{"api", "worker", "operator", "unrecognized"} {
		pod := map[string]string{"app.kubernetes.io/component": component, "goobers.dev/role": component}
		for _, address := range []string{"1.1.1.1", "169.254.169.254", "10.1.2.3"} {
			if controlPlanePoliciesAllowExternalTCP(t, policies, pod, address, 443) {
				t.Errorf("policy union gives %s direct HTTPS to %s", component, address)
			}
		}
	}
	proxy := map[string]string{"app.kubernetes.io/name": "goobers-egress-proxy"}
	if !controlPlanePoliciesAllowExternalTCP(t, policies, proxy, "1.1.1.1", 443) {
		t.Fatal("proxy cannot reach public HTTPS")
	}
	for _, address := range []string{"10.1.2.3", "172.16.1.2", "192.168.1.2", "169.254.169.254", "127.0.0.1", "fd00::1"} {
		if controlPlanePoliciesAllowExternalTCP(t, policies, proxy, address, 443) {
			t.Errorf("policy union gives proxy HTTPS to %s", address)
		}
	}
	if controlPlanePoliciesAllowExternalTCP(t, policies, proxy, "1.1.1.1", 80) {
		t.Fatal("proxy public HTTP is not denied")
	}
	// Negative control: another selecting policy unions over default-deny.
	rogue := networkingv1.NetworkPolicy{Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, Egress: []networkingv1.NetworkPolicyEgressRule{{}}}}
	if !controlPlanePoliciesAllowExternalTCP(t, append(policies, rogue), proxy, "169.254.169.254", 443) {
		t.Fatal("test evaluator failed to detect an additive allow-all bypass")
	}
}

// External destinations cannot match pod/namespace peers. Evaluate every
// selecting egress rule, not just the named proxy policy, to catch additive
// grants that would nullify its exclusions. This is a manifest assertion, not
// a claim that a live CNI enforces the rendered policy.
func controlPlanePoliciesAllowExternalTCP(t *testing.T, policies []networkingv1.NetworkPolicy, pod map[string]string, address string, port int) bool {
	t.Helper()
	ip := netip.MustParseAddr(address)
	for _, policy := range policies {
		selector, err := metav1.LabelSelectorAsSelector(&policy.Spec.PodSelector)
		if err != nil {
			t.Fatal(err)
		}
		if !selector.Matches(labels.Set(pod)) || !slices.Contains(policy.Spec.PolicyTypes, networkingv1.PolicyTypeEgress) {
			continue
		}
		for _, rule := range policy.Spec.Egress {
			portAllowed := len(rule.Ports) == 0
			for _, p := range rule.Ports {
				if (p.Protocol == nil || string(*p.Protocol) == "TCP") && (p.Port == nil || p.Port.IntValue() == port) {
					portAllowed = true
				}
			}
			if portAllowed && externalPeerAllows(t, rule.To, ip) {
				return true
			}
		}
	}
	return false
}

func externalPeerAllows(t *testing.T, peers []networkingv1.NetworkPolicyPeer, ip netip.Addr) bool {
	t.Helper()
	if len(peers) == 0 {
		return true
	}
	for _, peer := range peers {
		if peer.IPBlock == nil {
			if peer.PodSelector == nil && peer.NamespaceSelector == nil {
				return true
			}
			continue
		}
		prefix, err := netip.ParsePrefix(peer.IPBlock.CIDR)
		if err != nil {
			t.Fatal(err)
		}
		allowed := prefix.Contains(ip)
		for _, excluded := range peer.IPBlock.Except {
			prefix, err := netip.ParsePrefix(excluded)
			if err != nil {
				t.Fatal(err)
			}
			allowed = allowed && !prefix.Contains(ip)
		}
		if allowed {
			return true
		}
	}
	return false
}

func TestDeployReferenceProxyPublicEgressCannotReachPrivateDestinations(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/reference/goobers-system/networkpolicies.yaml")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, doc := range splitYAMLDocs(raw) {
		var policy networkingv1.NetworkPolicy
		if err := yaml.UnmarshalStrict(doc, &policy); err != nil {
			t.Fatal(err)
		}
		if policy.Name != "allow-proxy-public-https" {
			continue
		}
		checked++
		if policy.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"] != "goobers-egress-proxy" || len(policy.Spec.Egress) != 1 {
			t.Fatalf("Internet grant is not confined to the proxy: %+v", policy.Spec)
		}
		rule := policy.Spec.Egress[0]
		if len(rule.Ports) != 1 || rule.Ports[0].Port == nil || rule.Ports[0].Port.IntValue() != 443 || rule.Ports[0].Protocol == nil || string(*rule.Ports[0].Protocol) != "TCP" || len(rule.To) != 1 || rule.To[0].IPBlock == nil {
			t.Fatalf("Internet grant is not just TCP443 to one filtered IP block: %+v", rule)
		}
		block := rule.To[0].IPBlock
		if block.CIDR != "0.0.0.0/0" {
			t.Fatalf("unexpected public address family: %+v", block)
		}
		for _, address := range []string{"10.1.2.3", "172.16.2.3", "172.31.255.254", "192.168.1.2", "169.254.169.254", "169.254.1.1", "127.0.0.1", "100.64.1.1"} {
			denied := false
			for _, exclusion := range block.Except {
				prefix, err := netip.ParsePrefix(exclusion)
				if err != nil {
					t.Fatal(err)
				}
				denied = denied || prefix.Contains(netip.MustParseAddr(address))
			}
			if !denied {
				t.Errorf("proxy can reach forbidden destination %s", address)
			}
		}
	}
	if checked != 1 {
		t.Fatalf("checked %d public proxy grants, want exactly one", checked)
	}
}
