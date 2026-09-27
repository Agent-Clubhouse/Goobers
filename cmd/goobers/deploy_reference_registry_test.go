package main

import (
	"os"
	"path/filepath"
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
// overlay still demonstrates the placeholder pattern and is wired into
// `make deploy-validate`'s render/kubeconform lines (checked separately
// below; this test does not itself invoke kubectl or kubeconform).
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

	// Every *-deployment.yaml the base ships, not a fixed subset — matches
	// the glob cmd/goobers/deploy_reference_test.go already uses so a new
	// deployment manifest can't silently go unchecked here.
	paths, err := filepath.Glob("../../deploy/reference/goobers-system/*-deployment.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("no *-deployment.yaml manifests found under goobers-system")
	}
	for _, path := range paths {
		name := filepath.Base(path)
		if !slices.Contains(kustomization.Resources, name) {
			t.Fatalf("goobers-system/kustomization.yaml does not list %q", name)
		}
		depRaw, err := os.ReadFile(path)
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

	// This only pins that the Makefile lines exist, not that kubectl/
	// kubeconform actually succeed against them — `make deploy-validate`
	// itself is what renders and kubeconform-checks the overlay.
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
			t.Errorf("Makefile's deploy-validate target is missing %q — make deploy-validate would not render or schema-check the example overlay", want)
		}
	}
}
