package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"sigs.k8s.io/yaml"
)

func TestDeployReferenceConfigMirrorAliasesStatelessBase(t *testing.T) {
	kustomization, err := os.ReadFile("../../deploy/reference/config-mirror/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(kustomization), "mirror-pvc") || strings.Contains(string(kustomization), "worker-linux") {
		t.Fatal("compatibility overlay restores the retired shared config mirror")
	}
	raw, err := os.ReadFile(filepath.Join("../../deploy/reference/goobers-system", "worker-deployment.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var deployment appsv1.Deployment
	if err := yaml.Unmarshal(raw, &deployment); err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas < 2 {
		t.Fatalf("worker replicas = %v, want at least 2", deployment.Spec.Replicas)
	}
	privateWorkRoot := false
	for _, volume := range deployment.Spec.Template.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			t.Fatalf("worker base retained PVC %q", volume.PersistentVolumeClaim.ClaimName)
		}
		if volume.Name == "workspaces" && volume.EmptyDir != nil {
			privateWorkRoot = true
		}
	}
	if !privateWorkRoot {
		t.Fatal("worker work root is not backed by emptyDir")
	}
}
