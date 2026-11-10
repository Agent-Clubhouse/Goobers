package workflowgym

import (
	"path/filepath"
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

func TestLoadDigestIndependentOfFileNameOrder(t *testing.T) {
	const wantDigest = "sha256:f0ec70aff2ac7be6e3227d0143d134d4d553c267a5571de21b4c53b95ff5d439"
	want, err := Load("testdata/valid")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Load("testdata/reversed-names")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tasks) != len(want.Tasks) {
		t.Fatalf("task count = %d, want %d", len(got.Tasks), len(want.Tasks))
	}
	for i := range want.Tasks {
		if got.Tasks[i].TaskID != want.Tasks[i].TaskID {
			t.Fatalf("task %d = %q, want %q", i, got.Tasks[i].TaskID, want.Tasks[i].TaskID)
		}
	}
	if got.Digest != want.Digest {
		t.Fatalf("digest = %q, want %q", got.Digest, want.Digest)
	}
	if want.Digest != wantDigest {
		t.Fatalf("pinned digest = %q, want %q", want.Digest, wantDigest)
	}
}

func TestLoadRejectsMissingOrInvalidSplit(t *testing.T) {
	for _, dir := range []string{"testdata/missing-split", "testdata/invalid-split"} {
		t.Run(dir, func(t *testing.T) {
			_, err := Load(dir)
			if err == nil {
				t.Fatal("Load succeeded, want split error")
			}
			// Strip the fixture path so its directory name cannot satisfy the assertion.
			msg := strings.ReplaceAll(err.Error(), filepath.FromSlash(dir), "")
			if !strings.Contains(msg, "split") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
