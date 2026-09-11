package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runnercap"
)

// readMultiDoc splits a multi-document YAML file into raw maps, mirroring
// ../authenticated/main.go's readDocs — that package cannot be imported here
// (unexported), so this is the same small, well-tested splitting logic
// re-applied to the hand-authored reference tree instead of a rendered one.
func readMultiDoc(t *testing.T, path string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var docs []map[string]any
	for _, raw := range bytes.Split(b, []byte("\n---")) {
		var d map[string]any
		if err := yaml.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		if d["kind"] != nil {
			docs = append(docs, d)
		}
	}
	return docs
}

func decodeInto(t *testing.T, d map[string]any, out any) {
	t.Helper()
	b, err := yaml.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(b, out); err != nil {
		t.Fatal(err)
	}
}

// matchLabelsSatisfiedBy reports whether every key/value in want is present
// and equal in have — a MatchLabels selector is satisfied by a superset.
func matchLabelsSatisfiedBy(want, have map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func hasTCPPort(ports []networkingv1.NetworkPolicyPort, port int32) bool {
	for _, p := range ports {
		if p.Port != nil && p.Port.IntVal == port && (p.Protocol == nil || *p.Protocol == corev1.ProtocolTCP) {
			return true
		}
	}
	return false
}

// TestDaemonIngressAdmitsRealWorkerAndStagePods is #4828's regression: every
// in-cluster caller the daemon's own reference collateral documents — the
// worker Deployment and a REAL dispatcher-rendered stage pod (not a
// hand-copied label set, so this drifts with the dispatcher rather than
// silently diverging from it) — must be admitted by some rendered reference
// NetworkPolicy selecting the daemon (`app.kubernetes.io/component: api`) on
// its API/blob-plane port. Before #4828 this port had ingress from nothing:
// every dispatched stage failed closed at surrender, retrying rather than
// surfacing as a visible error.
func TestDaemonIngressAdmitsRealWorkerAndStagePods(t *testing.T) {
	const daemonPort = 8080 // internal/netpolrender.DefaultBlobEndpoint().Port

	// Real worker pod-template labels, from the actual reference Deployment.
	var workerDeploy appsv1.Deployment
	for _, d := range readMultiDoc(t, "worker-deployment.yaml") {
		if d["kind"] == "Deployment" {
			decodeInto(t, d, &workerDeploy)
		}
	}
	if len(workerDeploy.Spec.Template.Labels) == 0 {
		t.Fatal("worker-deployment.yaml: could not read pod template labels")
	}

	// Real gaggle-namespace labels, from the actual reference Namespace
	// template — not a hand-copied constant, so a future rename of the
	// marker label (or a forgotten one) fails this test instead of shipping
	// silently.
	var gaggleNS corev1.Namespace
	for _, d := range readMultiDoc(t, "../gaggle-namespace/base/namespace.yaml") {
		if d["kind"] == "Namespace" {
			decodeInto(t, d, &gaggleNS)
		}
	}
	if gaggleNS.Labels[runnercap.LabelGaggleNamespace] != runnercap.GaggleNamespaceMarker {
		t.Fatalf("gaggle-namespace/base/namespace.yaml must carry %s=%s (#4828): a working ingress grant "+
			"has no other selector to match a gaggle namespace generically", runnercap.LabelGaggleNamespace, runnercap.GaggleNamespaceMarker)
	}

	// A REAL dispatcher-rendered stage pod — same labels a live mode-3 stage
	// pod would carry, derived from the actual dispatcher code rather than
	// hand-copied (matches ../authenticated/main_test.go's
	// TestPreparedTopologyNetworkPoliciesMatchRealStageLabels pattern).
	pod, err := dispatcher.RenderPod(
		dispatcher.Config{Namespace: "change-me-gaggle"},
		dispatcher.Attempt{RunID: "run-4828", Stage: "probe", Number: 1},
		dispatcher.RunnerSpec{
			Name: "linux-pod", OS: "linux", HostKind: instance.RunnerHostImage,
			Host:         "registry.example.test/goobers@sha256:" + strings.Repeat("a", 64),
			Restrictions: []string{string(runnercap.RestrictionNetworkNone)},
		},
	)
	if err != nil {
		t.Fatalf("RenderPod: %v", err)
	}
	if pod.Labels[runnercap.LabelRole] != runnercap.RoleStage {
		t.Fatalf("dispatcher.RenderPod did not stamp %s=%s; test fixture is stale against the dispatcher", runnercap.LabelRole, runnercap.RoleStage)
	}

	var (
		workerAdmitted, stageAdmitted bool
		defaultDenies                 int
	)
	for _, d := range readMultiDoc(t, "networkpolicies.yaml") {
		if d["kind"] != "NetworkPolicy" {
			continue
		}
		var p networkingv1.NetworkPolicy
		decodeInto(t, d, &p)

		if p.Name == "default-deny-all" {
			if len(p.Spec.PolicyTypes) != 2 || len(p.Spec.Ingress) != 0 || len(p.Spec.Egress) != 0 {
				t.Fatal("default-deny-all lost its deny-first shape")
			}
			defaultDenies++
			continue
		}

		selectsDaemon := matchLabelsSatisfiedBy(map[string]string{"app.kubernetes.io/component": "api"}, p.Spec.PodSelector.MatchLabels)
		if !selectsDaemon {
			continue
		}
		for _, rule := range p.Spec.Ingress {
			if !hasTCPPort(rule.Ports, daemonPort) {
				continue
			}
			for _, peer := range rule.From {
				switch {
				case peer.NamespaceSelector == nil && peer.PodSelector != nil:
					if matchLabelsSatisfiedBy(peer.PodSelector.MatchLabels, workerDeploy.Spec.Template.Labels) {
						workerAdmitted = true
					}
				case peer.NamespaceSelector != nil && peer.PodSelector != nil:
					// Decision-012 AND composition: both selectors must live
					// in this ONE peer element (checked structurally by
					// reaching this branch at all — two separate list
					// elements would each fail one half and never both
					// match here), and both must be satisfied by the real
					// objects for this to count as admitting anything.
					if matchLabelsSatisfiedBy(peer.NamespaceSelector.MatchLabels, gaggleNS.Labels) &&
						matchLabelsSatisfiedBy(peer.PodSelector.MatchLabels, pod.Labels) {
						stageAdmitted = true
					}
				}
			}
		}
	}

	if defaultDenies != 1 {
		t.Fatalf("default-deny-all count = %d, want 1", defaultDenies)
	}
	if !workerAdmitted {
		t.Fatal("no rendered NetworkPolicy admits the real worker Deployment's pods to the daemon on port 8080 (#4828)")
	}
	if !stageAdmitted {
		t.Fatal("no rendered NetworkPolicy admits a real dispatcher-rendered stage pod, in a labeled gaggle namespace, to the daemon on port 8080 (#4828)")
	}
}
