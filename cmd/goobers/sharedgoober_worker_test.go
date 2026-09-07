package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/localscheduler"
)

func sharedWorkerFixture(t *testing.T) (string, string) {
	t.Helper()
	root := initDemo(t)
	shared := filepath.Join(root, "goobers", pinGoober)
	if err := os.MkdirAll(filepath.Dir(shared), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "config", "gaggles", pinGaggle, "goobers", pinGoober), shared); err != nil {
		t.Fatal(err)
	}
	definition := filepath.Join(shared, "goober.yaml")
	writeFileContent(t, definition, strings.Replace(readFileContent(t, definition), "  gaggle: example\n", "", 1))
	instructions := filepath.Join(shared, "instructions.md")
	return root, instructions
}

func TestWorkerRetainsSharedPersonaAcrossInstructionReload(t *testing.T) {
	root, instructions := sharedWorkerFixture(t)
	admitted := readFileContent(t, instructions)
	seams := workerReloadSeams(t, root)
	oldPin := currentPin(t, seams)
	snapshot := seams.snapshot.Load()
	_, daemonPins, _, _, err := compiledMachinesWithGooberDigestsAndWarnings(
		instance.NewLayout(root).ConfigDir(), snapshot.set, goobersByName(snapshot.set), snapshot.instructions,
		snapshot.cfg.Runner.EnvPassthrough, snapshot.cfg.Runner.HarnessCommand, true, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if daemonPins[localscheduler.WorkflowIdentity{Gaggle: pinGaggle, Workflow: pinWorkflow}] != oldPin {
		t.Fatal("daemon and worker disagree on the shared persona pin")
	}
	oldKit, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, oldPin)
	if err != nil {
		t.Fatal(err)
	}
	writeFileContent(t, instructions, admitted+"\nShared persona changed after admission.\n")
	reloadApplied(t, seams)
	if currentPin(t, seams) == oldPin {
		t.Fatal("shared instruction edit was invisible to reload and pinning")
	}
	again, err := seams.forPinnedGaggle(pinGaggle, pinWorkflow, oldPin)
	if err != nil || again != oldKit {
		t.Fatalf("retained shared persona kit changed: %v", err)
	}
	writer := agenticKitWriter{instanceRoot: root, seams: seams, blobEndpoint: "http://blobs.invalid"}
	kit, err := writer.buildKit(apiv1.InvocationEnvelope{
		TaskID: "shared-run:implement", RunID: "shared-run", WorkflowID: pinWorkflow,
		Gaggle: pinGaggle, Goober: pinGoober, GooberDigest: oldPin,
	}, "invoke")
	if err != nil {
		t.Fatal(err)
	}
	if kit.Instructions[pinGoober] != admitted {
		t.Fatal("pod kit replaced the admitted shared instructions with current disk content")
	}
}

func TestWorkerSnapshotsSharedSkillContentBeforeReload(t *testing.T) {
	root, _ := sharedWorkerFixture(t)
	shared := filepath.Join(root, "skills", "implement", "SKILL.md")
	local := filepath.Join(root, "config", "gaggles", pinGaggle, "skills", "implement", "SKILL.md")
	for _, path := range []string{shared, local} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFileContent(t, shared, "shared before")
	writeFileContent(t, local, "gaggle override")
	seams := workerReloadSeams(t, root)
	oldPin := currentPin(t, seams)
	old := seams.snapshot.Load()
	// Changing only the unrelated gaggle override cannot alter this shared
	// persona's identity. Shared content is captured at snapshot admission.
	writeFileContent(t, local, "another gaggle override")
	candidate, _, err := seams.loadConfigSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := candidate.gooberDigestFor(pinGaggle, pinWorkflow); err != nil || got != oldPin {
		t.Fatalf("shared pin changed with unrelated gaggle content: %q, %v", got, err)
	}
	writeFileContent(t, shared, "shared after")
	reloadApplied(t, seams)
	if currentPin(t, seams) == oldPin {
		t.Fatal("shared skill edit did not change the new snapshot's pin")
	}
	if got, err := old.gooberDigestFor(pinGaggle, pinWorkflow); err != nil || got != oldPin {
		t.Fatalf("historical shared skill pin followed disk: %q, %v", got, err)
	}
}

func TestWorkerSharesOnePersonaBetweenTwoGaggles(t *testing.T) {
	root, instructions := sharedWorkerFixture(t)
	config := instance.NewLayout(root).ConfigDir()
	second := filepath.Join(config, "gaggles", "second")
	if err := os.CopyFS(second, os.DirFS(filepath.Join(config, "gaggles", pinGaggle))); err != nil {
		t.Fatal(err)
	}
	gaggle := filepath.Join(second, "gaggle.yaml")
	text := strings.Replace(readFileContent(t, gaggle), "  name: example\n", "  name: second\n", 1)
	writeFileContent(t, gaggle, strings.ReplaceAll(text, "gaggle-example", "gaggle-second"))
	flow := filepath.Join(second, "workflows", pinWorkflow+".yaml")
	writeFileContent(t, flow, strings.Replace(readFileContent(t, flow), "  gaggle: example\n", "  gaggle: second\n", 1))
	manifest := filepath.Join(config, "manifest.yaml")
	writeFileContent(t, manifest, readFileContent(t, manifest)+"    - second\n")
	writeFileContent(t, filepath.Join(root, "instance.yaml.example"), readFileContent(t, filepath.Join(root, "instance.yaml")))
	if code, stdout, stderr := runArgs(t, "validate", "--source-tree", root); code != 0 {
		t.Fatalf("checked-in shared source failed validation: %d\n%s\n%s", code, stdout, stderr)
	}
	seams := workerReloadSeams(t, root)
	firstPin := currentPin(t, seams)
	secondPin, err := seams.snapshot.Load().gooberDigestFor("second", pinWorkflow)
	if err != nil || firstPin != secondPin {
		t.Fatalf("same persona acquired gaggle-specific identities: %s / %s (%v)", firstPin, secondPin, err)
	}
	writer := agenticKitWriter{instanceRoot: root, seams: seams, blobEndpoint: "http://blobs.invalid"}
	for _, gaggle := range []string{pinGaggle, "second"} {
		kit, err := writer.buildKit(apiv1.InvocationEnvelope{
			TaskID: gaggle + ":implement", RunID: gaggle, WorkflowID: pinWorkflow,
			Gaggle: gaggle, Goober: pinGoober, GooberDigest: firstPin,
		}, "invoke")
		if err != nil {
			t.Fatal(err)
		}
		if kit.Instructions[pinGoober] != readFileContent(t, instructions) {
			t.Fatalf("%s did not receive the shared persona", gaggle)
		}
	}
}
