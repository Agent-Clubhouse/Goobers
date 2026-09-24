package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// #3287: the goobers-system base used to hardcode a `registry.example.com/
// CHANGE-ME` `images:` transformer inside the base itself. A remote-base
// consumer (pinned, unforked, so upstream fixes flow) cannot edit the base,
// and by the time its own overlay's `images:` transformer ran, the base's
// transformer had already rewritten every container off the bare `goobers`
// name — so the consumer's own `- name: goobers` overlay silently matched
// nothing and never retargeted the containers.
//
// The fix moves the placeholder transformer out of the base entirely, into
// an example overlay a fork-and-edit consumer copies. This pins both halves
// of that contract: the base stays bare and transformable, and the example
// overlay still demonstrates (and exercises, via `make deploy-validate`) the
// placeholder pattern.
func TestDeployReferenceGoobersSystemBaseLeavesImageBare(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/reference/goobers-system/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var kustomization struct {
		Images []struct {
			Name    string `json:"name"`
			NewName string `json:"newName"`
			NewTag  string `json:"newTag"`
		} `json:"images"`
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(raw, &kustomization); err != nil {
		t.Fatal(err)
	}
	if len(kustomization.Images) != 0 {
		t.Fatalf("goobers-system/kustomization.yaml declares an images: transformer %+v — a remote-base consumer cannot edit the base, and this shadows the consumer's own images: overlay", kustomization.Images)
	}

	for _, name := range []string{"operator-deployment.yaml", "worker-deployment.yaml", "api-deployment.yaml"} {
		if !slices.Contains(kustomization.Resources, name) {
			t.Fatalf("goobers-system/kustomization.yaml does not list %q", name)
		}
		depRaw, err := os.ReadFile("../../deploy/reference/goobers-system/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(depRaw), "image: goobers") {
			t.Errorf("%s does not reference the bare image name %q", name, "goobers")
		}
	}
}

func TestDeployReferenceGoobersSystemRegistryExampleRetargetsBareImage(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/reference/examples/goobers-system-registry/kustomization.yaml")
	if err != nil {
		t.Fatalf("deploy/reference/examples/goobers-system-registry has no kustomization.yaml: %v", err)
	}
	var kustomization struct {
		Resources []string `json:"resources"`
		Images    []struct {
			Name    string `json:"name"`
			NewName string `json:"newName"`
			NewTag  string `json:"newTag"`
		} `json:"images"`
	}
	if err := yaml.Unmarshal(raw, &kustomization); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(kustomization.Resources, "../../goobers-system") {
		t.Fatalf("goobers-system-registry example does not use goobers-system as its base: resources = %v", kustomization.Resources)
	}
	found := false
	for _, image := range kustomization.Images {
		if image.Name == "goobers" && image.NewName != "" && image.NewTag != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("goobers-system-registry example does not retarget the bare %q image: images = %+v", "goobers", kustomization.Images)
	}

	makefile, err := os.ReadFile("../../Makefile")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(makefile), "\n")
	for _, want := range []string{
		"\tkubectl kustomize deploy/reference/examples/goobers-system-registry >/dev/null",
		"\tkubectl kustomize deploy/reference/examples/goobers-system-registry | $(KUBECONFORM) -strict -summary",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("Makefile's deploy-validate target is missing %q — the example overlay is not rendered or schema-checked", want)
		}
	}
}
