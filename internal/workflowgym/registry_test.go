package workflowgym

import (
	"strings"
	"testing"
)

func TestLoadOrdersTasksAndProducesStableDigest(t *testing.T) {
	first, err := Load("testdata/valid")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load("testdata/valid")
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digest changed: %q != %q", first.Digest, second.Digest)
	}
	if len(first.Tasks) != 2 || first.Tasks[0].TaskID != "task-alpha" || first.Tasks[1].TaskID != "task-beta" {
		t.Fatalf("tasks are not in task ID order: %+v", first.Tasks)
	}
	if first.DatasetVersion != "2026.1" {
		t.Fatalf("dataset version = %q", first.DatasetVersion)
	}
}

func TestLoadRejectsMissingProvenance(t *testing.T) {
	_, err := Load("testdata/missing-provenance")
	if err == nil || !strings.Contains(err.Error(), "provenance") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadRejectsDuplicateTaskIDs(t *testing.T) {
	_, err := Load("testdata/duplicate")
	if err == nil || !strings.Contains(err.Error(), `duplicate task ID "repeated-task"`) {
		t.Fatalf("error = %v", err)
	}
}
