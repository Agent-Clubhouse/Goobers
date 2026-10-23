package workerrollout

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestDeploymentStatusReadsBlockedTarget(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"versionCount": int64(3),
			"targetVersion": map[string]any{
				"buildID": "v4",
			},
			"currentVersion": map[string]any{
				"buildID": "v3",
			},
			"deprecatedVersions": []any{
				map[string]any{"buildID": "v1", "status": "Draining"},
				map[string]any{"buildID": "v2", "status": "Drained", "eligibleForDeletion": true},
			},
		},
	}}
	status, err := deploymentStatus(object)
	if err != nil {
		t.Fatal(err)
	}
	if status.TargetBuildID != "v4" || status.TargetHasDeployment ||
		status.CurrentBuildID != "v3" || status.VersionCount != 3 ||
		len(status.Deprecated) != 2 || status.Deprecated[0].BuildID != "v1" ||
		!status.Deprecated[1].EligibleForDeletion {
		t.Fatalf("status = %+v", status)
	}

	object.Object["status"].(map[string]any)["targetVersion"].(map[string]any)["deployment"] =
		map[string]any{"name": "goobers-v4"}
	status, err = deploymentStatus(object)
	if err != nil {
		t.Fatal(err)
	}
	if !status.TargetHasDeployment {
		t.Fatal("target deployment was not detected")
	}
}

func TestDeploymentStatusRejectsMalformedDeprecatedVersion(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"deprecatedVersions": []any{"not-an-object"},
		},
	}}
	if _, err := deploymentStatus(object); err == nil {
		t.Fatal("deploymentStatus accepted malformed status")
	}
}
