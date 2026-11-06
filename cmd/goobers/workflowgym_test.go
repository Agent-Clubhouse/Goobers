package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowGymListPrintsDigestCountAndStableOrder(t *testing.T) {
	path := filepath.Join("..", "..", "internal", "workflowgym", "testdata", "valid")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"workflow-gym", "list", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{"datasetVersion: 2026.1", "datasetDigest: sha256:", "taskCount: 2"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
	if strings.Index(output, "task-alpha") > strings.Index(output, "task-beta") {
		t.Fatalf("tasks not sorted by ID:\n%s", output)
	}
}

func TestWorkflowGymValidateRejectsInvalidFixture(t *testing.T) {
	path := filepath.Join("..", "..", "internal", "workflowgym", "testdata", "missing-provenance")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"workflow-gym", "validate", path}, &stdout, &stderr); code != 1 {
		t.Fatalf("code = %d, stdout = %s, stderr = %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "provenance") {
		t.Fatalf("stderr = %s", stderr.String())
	}
}
