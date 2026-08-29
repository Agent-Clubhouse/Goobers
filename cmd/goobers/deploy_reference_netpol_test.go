package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/netpolrender"
	"github.com/goobers/goobers/internal/runnercap"
)

// TestDeployReferenceRenderedTogether is the #3301 rendered-together
// cross-base assertion, wired into deploy-validate (goobernetes-restrictions.md
// AC-6, D7): the goobers-system and gaggle-namespace bases are composed
// TOGETHER with the per-runner-class output of `goobers netpol-render`, and
//
//  1. every goobers.dev/role / goobers.dev/runner-class label rendered on a
//     pod anywhere (base pod templates + the label set the dispatcher stamps
//     on stage pods) is matched by a policy rendered somewhere;
//  2. every goobers.dev-keyed selector a policy or peer carries matches a pod
//     label set rendered somewhere — a selector nothing produces is the
//     silent no-grant (case A of dispatcher §2a: fails closed into a hang,
//     not a visible denial);
//  3. no Egress policy grants to goobers.dev/role=stage WITHOUT pinning a
//     runner-class label: composition is additive (delivery decision 004), so
//     a generic role-wide grant would make every narrower per-class policy a
//     no-op — the exact defect class #3301 named.
func TestDeployReferenceRenderedTogether(t *testing.T) {
	// The composed static bases.
	var files []string
	for _, glob := range []string{
		"../../deploy/reference/goobers-system/*.yaml",
		"../../deploy/reference/gaggle-namespace/base/*.yaml",
		"../../deploy/reference/temporal/*.yaml",
	} {
		matches, err := filepath.Glob(glob)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	if len(files) == 0 {
		t.Fatal("composed no reference manifests")
	}

	var podLabelSets []map[string]string
	var policies []*networkingv1.NetworkPolicy
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, doc := range splitYAMLDocs(raw) {
			var meta struct {
				Kind string `json:"kind"`
			}
			if err := yaml.Unmarshal(doc, &meta); err != nil || meta.Kind == "" {
				continue
			}
			switch meta.Kind {
			case "Deployment":
				var deployment appsv1.Deployment
				if err := yaml.Unmarshal(doc, &deployment); err != nil {
					t.Fatalf("parse Deployment in %s: %v", path, err)
				}
				podLabelSets = append(podLabelSets, deployment.Spec.Template.Labels)
			case "NetworkPolicy":
				var policy networkingv1.NetworkPolicy
				if err := yaml.Unmarshal(doc, &policy); err != nil {
					t.Fatalf("parse NetworkPolicy in %s: %v", path, err)
				}
				policies = append(policies, &policy)
			}
		}
	}

	// The rendered-together half: a representative inventory covering the
	// three class shapes, rendered through the real renderer, plus the label
	// sets the dispatcher stamps for the SAME inventory (runnercap is the
	// shared producer, decision 015).
	input := netpolrender.Input{
		Runners: []netpolrender.Runner{
			{Name: "ci-linux", Restrictions: []string{"network:allowlist", "fs:readonly-except-workspace", "tmp:ephemeral"}},
			{Name: "locked", Restrictions: []string{"network:none"}},
			{Name: "open", Restrictions: nil},
		},
		Allowlist: []netpolrender.AllowlistGroup{
			{Name: "github-provider", Kind: netpolrender.GroupKindProvider, CIDRs: []string{"140.82.112.0/20"}},
		},
	}
	rendered, err := netpolrender.Render(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range rendered.Classes {
		policies = append(policies, rendered.Policies[class.Value])
		podLabelSets = append(podLabelSets, map[string]string{
			runnercap.LabelRole:        runnercap.RoleStage,
			runnercap.LabelRunnerClass: runnercap.RunnerClassValue(class.Restrictions),
		})
	}

	goobersKeyed := func(labels map[string]string) map[string]string {
		out := map[string]string{}
		for key, value := range labels {
			if strings.HasPrefix(key, "goobers.dev/") {
				out[key] = value
			}
		}
		return out
	}
	subset := func(selector, labels map[string]string) bool {
		for key, value := range selector {
			if labels[key] != value {
				return false
			}
		}
		return true
	}

	// (1) Every goobers.dev-labeled pod is matched by a policy whose
	// podSelector names at least one goobers.dev key.
	for _, labels := range podLabelSets {
		keyed := goobersKeyed(labels)
		if len(keyed) == 0 {
			continue
		}
		var matched bool
		for _, policy := range policies {
			selector := goobersKeyed(policy.Spec.PodSelector.MatchLabels)
			if len(selector) > 0 && subset(selector, labels) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("pod labels %v are matched by no rendered policy's goobers.dev selector (composed bases + netpol-render output)", keyed)
		}
	}

	// (2) Every goobers.dev-keyed selector — spec.podSelector or a peer —
	// matches some rendered pod label set.
	assertSelectorProduced := func(where string, selector map[string]string) {
		keyed := goobersKeyed(selector)
		if len(keyed) == 0 {
			return
		}
		for _, labels := range podLabelSets {
			if subset(keyed, labels) {
				return
			}
		}
		t.Errorf("%s selects %v, which no rendered pod labels satisfy — the policy grants nothing (the silent no-grant shape)", where, keyed)
	}
	for _, policy := range policies {
		assertSelectorProduced("policy "+policy.Name+" podSelector", policy.Spec.PodSelector.MatchLabels)
		for _, rule := range policy.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.PodSelector != nil {
					assertSelectorProduced("policy "+policy.Name+" ingress peer", peer.PodSelector.MatchLabels)
				}
			}
		}
		for _, rule := range policy.Spec.Egress {
			for _, peer := range rule.To {
				if peer.PodSelector != nil {
					assertSelectorProduced("policy "+policy.Name+" egress peer", peer.PodSelector.MatchLabels)
				}
			}
		}
	}

	// (3) The additive-composition guard: an Egress policy selecting
	// role=stage must pin a runner-class label. A generic stage-wide egress
	// grant (the base's former allow-stage-egress) would union over — and so
	// nullify — every per-class grant.
	for _, policy := range policies {
		var hasEgress bool
		for _, policyType := range policy.Spec.PolicyTypes {
			if policyType == networkingv1.PolicyTypeEgress {
				hasEgress = true
			}
		}
		if !hasEgress || len(policy.Spec.Egress) == 0 {
			continue
		}
		match := policy.Spec.PodSelector.MatchLabels
		if match[runnercap.LabelRole] == runnercap.RoleStage && match[runnercap.LabelRunnerClass] == "" {
			t.Errorf("policy %s grants egress to goobers.dev/role=stage without pinning %s — "+
				"composition is additive (decision 004): this generic grant makes every per-class policy a no-op",
				policy.Name, runnercap.LabelRunnerClass)
		}
	}
}

var yamlDocSeparator = regexp.MustCompile(`(?m)^---\s*$`)

func splitYAMLDocs(raw []byte) [][]byte {
	var docs [][]byte
	for _, doc := range yamlDocSeparator.Split(string(raw), -1) {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		docs = append(docs, []byte(doc))
	}
	return docs
}
