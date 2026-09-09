package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/yaml"
)

func TestDeployReferenceConfigMirrorSeparatesWorkerStorage(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		t.Run(platform, func(t *testing.T) {
			base := "worker-deployment.yaml"
			if platform == "windows" {
				base = "worker-windows-deployment.yaml"
			}
			read := func(path string) []byte {
				data, err := os.ReadFile(filepath.Join("../../deploy/reference", path))
				if err != nil {
					t.Fatal(err)
				}
				data, err = yaml.YAMLToJSON(data)
				if err != nil {
					t.Fatal(err)
				}
				return data
			}
			data, err := strategicpatch.StrategicMergePatch(read("goobers-system/"+base), read("config-mirror/worker-"+platform+".yaml"), appsv1.Deployment{})
			if err != nil {
				t.Fatal(err)
			}
			var deployment appsv1.Deployment
			if err := json.Unmarshal(data, &deployment); err != nil {
				t.Fatal(err)
			}
			pod := deployment.Spec.Template.Spec
			foundPrivate, foundMirror := false, false
			for _, volume := range pod.Volumes {
				if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == "goobers-journal" {
					t.Fatal("worker retained daemon journal claim")
				}
				if volume.Name == "journal" {
					foundPrivate = volume.EmptyDir != nil
				}
				if volume.Name == "config-mirror" {
					foundMirror = volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ReadOnly
				}
			}
			if !foundPrivate || !foundMirror {
				t.Fatal("worker requires private instance and read-only mirror")
			}
			seed := pod.InitContainers[0]
			if len(seed.Args) == 0 || seed.Args[0] != "config-seed" {
				t.Fatal("init container does not validate a complete config seed")
			}
			if err := validateManifestArgs(seed.Args[1:], registeredCommandFlagSet(t, "config-seed"), []string{"mirror", "instance"}, 0); err != nil {
				t.Fatal(err)
			}
		})
	}
}
