package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

// temporalServerComponentPorts is the intra-cluster contract #4827's
// evidence measured on a live chart release: the TCP ports each OSS
// Temporal server component listens on for internal RPC (7233-7239),
// ringpop membership gossip (6933-6939), and metrics (9090). frontend's
// 7233 is also the external client-facing port
// (temporal-frontend-ingress.yaml's separate control-plane grant), but it
// is equally required intra-cluster: ringpop gossip and internal clients
// (worker/matching) reach every component, frontend included, on its own
// listen ports.
var temporalServerComponentPorts = map[string][]int32{
	"frontend": {6933, 7233, 9090},
	"history":  {6934, 7234, 9090},
	"matching": {6935, 7235, 9090},
	"worker":   {6939, 7239, 9090},
}

func readTemporalNetworkPolicies(t *testing.T) []networkingv1.NetworkPolicy {
	t.Helper()
	var policies []networkingv1.NetworkPolicy
	for _, name := range []string{"networkpolicy-temporal.yaml", "networkpolicy-temporal-intracluster.yaml"} {
		raw, err := os.ReadFile("../../deploy/reference/temporal/" + name)
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range splitYAMLDocs(raw) {
			var policy networkingv1.NetworkPolicy
			if err := yaml.UnmarshalStrict(doc, &policy); err != nil {
				t.Fatalf("parse NetworkPolicy in %s: %v", name, err)
			}
			if policy.Name == "" {
				continue
			}
			policies = append(policies, policy)
		}
	}
	if len(policies) == 0 {
		t.Fatal("found no NetworkPolicy in deploy/reference/temporal")
	}
	return policies
}

// TestDeployReferenceTemporalFrontendPolicyScopedToFrontendComponent is
// #4827's scoping half: temporal-frontend-ingress.yaml's podSelector must
// pin app.kubernetes.io/component=frontend, not just the chart-wide
// app.kubernetes.io/name=temporal every server pod (frontend, history,
// matching, worker, web, admintools) carries. Selecting on name alone made
// this the SOLE ingress rule matching history/matching/worker pods too,
// denying their own required RPC and gossip ports (#4827's actual bug).
func TestDeployReferenceTemporalFrontendPolicyScopedToFrontendComponent(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/reference/temporal/networkpolicy-temporal.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var policy networkingv1.NetworkPolicy
	if err := yaml.UnmarshalStrict(raw, &policy); err != nil {
		t.Fatal(err)
	}
	if got := policy.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"]; got != "frontend" {
		t.Fatalf("temporal-frontend-ingress podSelector app.kubernetes.io/component = %q, want %q — "+
			"an unscoped selector also governs history/matching/worker pods, denying their own required traffic (#4827)",
			got, "frontend")
	}
	if got := policy.Spec.PodSelector.MatchLabels["app.kubernetes.io/name"]; got != "temporal" {
		t.Fatalf("temporal-frontend-ingress podSelector app.kubernetes.io/name = %q, want %q", got, "temporal")
	}
}

// TestDeployReferenceTemporalNetworkPoliciesAdmitOwnIntraClusterTraffic is
// #4827's acceptance criterion: no reference NetworkPolicy selecting pods
// from a chart release may leave that release's own required intra-cluster
// traffic unadmitted. For each Temporal server component this synthesizes
// the pod labels the chart's Helm templates actually stamp, resolves every
// rendered policy whose podSelector matches those labels (the union of
// ingress ports a real pod with that label set receives — NetworkPolicy
// selectors watching the same pod are additive, never exclusive), and
// checks that union covers the component's own required ports.
//
// Against #4827's original single-policy state (podSelector matching only
// app.kubernetes.io/name=temporal, granting just port 7233) this test fails
// for history/matching/worker, which is exactly the incident: pods Running,
// frontend unreachable, because the components it depends on could not
// receive their own RPC or ringpop gossip.
func TestDeployReferenceTemporalNetworkPoliciesAdmitOwnIntraClusterTraffic(t *testing.T) {
	policies := readTemporalNetworkPolicies(t)

	components := make([]string, 0, len(temporalServerComponentPorts))
	for component := range temporalServerComponentPorts {
		components = append(components, component)
	}
	slices.Sort(components)

	for _, component := range components {
		requiredPorts := temporalServerComponentPorts[component]
		podLabels := labels.Set{
			"app.kubernetes.io/name":      "temporal",
			"app.kubernetes.io/component": component,
		}

		admitted := map[int32]bool{}
		for _, policy := range policies {
			selector, err := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
				MatchLabels:      policy.Spec.PodSelector.MatchLabels,
				MatchExpressions: policy.Spec.PodSelector.MatchExpressions,
			})
			if err != nil {
				t.Fatalf("policy %s has an invalid podSelector: %v", policy.Name, err)
			}
			if !selector.Matches(podLabels) {
				continue
			}
			for _, rule := range policy.Spec.Ingress {
				for _, p := range rule.Ports {
					if p.Port != nil {
						admitted[p.Port.IntVal] = true
					}
				}
			}
		}

		var missing []int32
		for _, port := range requiredPorts {
			if !admitted[port] {
				missing = append(missing, port)
			}
		}
		if len(missing) > 0 {
			t.Errorf("Temporal component %q (labels %v): no rendered NetworkPolicy admits its own required port(s) %v — "+
				"a pod with these labels is selected for Ingress but cannot receive its own cluster's traffic (#4827)",
				component, podLabels, missing)
		}
	}
}

// TestDeployReferenceTemporalHasKustomization is #4827's second acceptance
// criterion: deploy/reference/temporal was never rendered or schema-checked
// because it had no kustomization.yaml at all, so nothing in `make
// deploy-validate` ever caught the ingress-deny bug above. This pins the
// wiring, not just the file's existence.
func TestDeployReferenceTemporalHasKustomization(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/reference/temporal/kustomization.yaml")
	if err != nil {
		t.Fatalf("deploy/reference/temporal has no kustomization.yaml: %v", err)
	}
	var kustomization struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(raw, &kustomization); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"networkpolicy-temporal.yaml", "networkpolicy-temporal-intracluster.yaml"} {
		if !slices.Contains(kustomization.Resources, want) {
			t.Errorf("temporal/kustomization.yaml does not list %q — it is not rendered or schema-checked by make deploy-validate", want)
		}
	}
	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(strings.Split(string(makefile), "\n"), "\tkubectl kustomize deploy/reference/temporal | $(KUBECONFORM) -strict -summary") {
		t.Error("Makefile's deploy-validate target does not kubeconform-check deploy/reference/temporal")
	}
}
