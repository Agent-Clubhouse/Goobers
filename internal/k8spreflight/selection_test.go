package k8spreflight

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSelectedAPIServerCheckNeedsOnlyNetworkPolicyReads(t *testing.T) {
	dedicated := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "goobers-system", Labels: map[string]string{APIServerEgressLabel: "true"}}, Spec: networkingv1.NetworkPolicySpec{
		// Client ingress is unrelated to API-server egress, even on this policy.
		Ingress: []networkingv1.NetworkPolicyIngressRule{{From: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "192.0.2.0/24"}}}}},
		Egress:  []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "127.0.0.1/32"}}}}},
	}}
	provider := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "provider", Namespace: "gaggle-a"}, Spec: networkingv1.NetworkPolicySpec{Egress: []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "203.0.113.0/24"}}}}}}}
	client := fake.NewClientset(dedicated, provider)
	report := Run(context.Background(), client, Options{Checks: []string{"apiserver-ipblock-drift"}, APIServerEndpoint: "https://127.0.0.1"})
	if !report.Conformant || len(report.Results) != 1 || report.Results[0].Status != StatusPass {
		t.Fatalf("unrelated policies or checks affected scoped monitor: %+v", report)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "list" || action.GetResource().Resource != "networkpolicies" {
			t.Fatalf("least-privilege monitor made unrelated call: %v", action)
		}
	}
	// An exception removing the exact API endpoint must not count as an allow.
	dedicated.Spec.Egress[0].To[0].IPBlock.Except = []string{"127.0.0.1/32"}
	if _, err := client.NetworkingV1().NetworkPolicies(dedicated.Namespace).Update(context.Background(), dedicated, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	result := checkAPIServerIPBlockDrift(context.Background(), client, Options{APIServerEndpoint: "https://127.0.0.1"})
	if result.Status != StatusFail {
		t.Fatalf("excluded API endpoint was accepted: %+v", result)
	}
}

func TestUnknownCheckSelectionDoesNotProbe(t *testing.T) {
	for _, ids := range [][]string{{"typo"}, {"pod-health", "pod-health"}} {
		report := Run(context.Background(), nil, Options{Checks: ids})
		if report.Conformant || len(report.Results) != 1 || report.Results[0].ID != "check-selection" {
			t.Fatalf("invalid selection accepted: %+v", report)
		}
	}
}

func TestSelectedCheckReportDoesNotClaimFullClusterConformance(t *testing.T) {
	report := Run(context.Background(), nil, Options{Checks: []string{"registry"}})
	var out strings.Builder
	WriteText(&out, report)
	if strings.Contains(out.String(), "cluster conforms") || !strings.Contains(out.String(), "full cluster conformance was not assessed") {
		t.Fatalf("selected-only report overclaims coverage: %s", out.String())
	}
	out.Reset()
	if err := WriteJSON(&out, report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"selectedChecks"`) {
		t.Fatalf("JSON omits restricted coverage: %s", out.String())
	}
}

func TestAPIServerDNSReceivesBoundedProbeContext(t *testing.T) {
	result := checkAPIServerIPBlockDrift(context.Background(), nil, Options{
		APIServerEndpoint: "https://apiserver.example.invalid", Timeout: time.Second,
		LookupAPIServerIPs: func(ctx context.Context, host string) ([]net.IP, error) {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) <= 0 || time.Until(deadline) > time.Second {
				t.Fatalf("DNS context is unbounded: deadline=%v ok=%t", deadline, ok)
			}
			if host != "apiserver.example.invalid" {
				t.Fatalf("wrong DNS target %q", host)
			}
			return nil, context.DeadlineExceeded
		},
	})
	if result.Status != StatusFail || !strings.Contains(result.Detail, "deadline exceeded") {
		t.Fatalf("DNS timeout not reported: %+v", result)
	}
}
