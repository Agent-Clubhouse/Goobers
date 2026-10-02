package k8spreflight

import (
	"context"
	"github.com/goobers/goobers/internal/netpolrender"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"strings"
	"testing"
)

func TestNetworkNoneDNSNeverClaimsDataplaneProof(t *testing.T) {
	for _, keepDNS := range []bool{false, true} {
		rendered, err := netpolrender.Render(netpolrender.Input{Runners: []netpolrender.Runner{{Name: "locked", Restrictions: []string{"network:none"}}}, KeepDNSForNetworkNone: keepDNS})
		if err != nil {
			t.Fatal(err)
		}
		policy := rendered.Policies[rendered.Classes[0].Value]
		policy.Namespace = "gaggle"
		client := fake.NewClientset(policy)
		report := Run(context.Background(), client, Options{Checks: []string{"network-none-dns"}})
		result := report.Results[0]
		if !report.Conformant || result.Status != StatusWarn || !strings.Contains(result.Detail, "UNVERIFIED dataplane") || !strings.Contains(result.Detail, "gaggle/") {
			t.Fatalf("%+v", report)
		}
		if strings.Contains(result.Detail, "may permit DNS") != keepDNS {
			t.Fatalf("%+v", result)
		}
	}
}

func TestNetworkNoneDNSRecognizesBroadGrants(t *testing.T) {
	rendered, _ := netpolrender.Render(netpolrender.Input{Runners: []netpolrender.Runner{{Name: "locked", Restrictions: []string{"network:none"}}}})
	policy := rendered.Policies[rendered.Classes[0].Value]
	for _, ports := range [][]networkingv1.NetworkPolicyPort{nil, {{Port: func() *intstr.IntOrString { p := intstr.FromString("dns"); return &p }()}}} {
		policy.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{Ports: ports}}
		result := checkNetworkNoneDNS(context.Background(), fake.NewClientset(policy), Options{})
		if !strings.Contains(result.Detail, "may permit DNS") {
			t.Fatalf("%+v", result)
		}
	}
}
