package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/harness"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/workflow"
)

func TestWorkerKitDeliversCapturedSkillsThroughPodExecutor(t *testing.T) {
	for _, selected := range []apiv1.Harness{apiv1.HarnessClaudeCode, apiv1.HarnessCodex} {
		t.Run(string(selected), func(t *testing.T) {
			root := initDemo(t)
			layout := instance.NewLayout(root)
			definition := filepath.Join(layout.ConfigDir(), "gaggles", pinGaggle, "goobers", pinGoober, "goober.yaml")
			writeFileContent(t, definition, strings.Replace(readFileContent(t, definition), "harness: copilot", "harness: "+string(selected), 1))
			skill := filepath.Join(layout.ConfigDir(), "gaggles", pinGaggle, "skills", "implement", "SKILL.md")
			captured := readFileContent(t, skill) + "\nCaptured skill delivery marker.\n"
			writeFileContent(t, skill, captured)
			seams := workerReloadSeams(t, root)
			if _, err := seams.reloadOnce(); err != nil {
				t.Fatal(err)
			}
			// Only selected packages may cross the claim check. Later edits to
			// the config tree cannot replace the captured execution inputs.
			seams.snapshot.Load().skillPackages[pinGaggle]["unselected"] = []workflow.SkillFile{{Path: "SKILL.md", Content: "unselected-package-must-not-travel"}}
			writeFileContent(t, skill, "changed live source must not travel")
			endpoint, _ := fakeBlobPlane(t)
			writer := agenticKitWriter{instanceRoot: root, seams: seams, blobEndpoint: endpoint}
			env := apiv1.InvocationEnvelope{RunID: "skills-run", TaskID: "stage", WorkflowID: pinWorkflow, Gaggle: pinGaggle, Goober: pinGoober}
			digest, err := writer.WriteKit(t.Context(), dispatcher.Attempt{RunID: env.RunID, Stage: env.TaskID, Number: 1, Agentic: true, Envelope: &env})
			if err != nil {
				t.Fatal(err)
			}
			data, err := (&dispatcher.BlobClient{BaseURL: endpoint}).Get(t.Context(), digest)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(data, []byte("Captured skill delivery marker.")) || bytes.Contains(data, []byte("unselected-package-must-not-travel")) || bytes.Contains(data, []byte("changed live source must not travel")) {
				t.Fatal("worker kit omitted captured skills or disclosed unselected/mutable bytes")
			}
			kit, err := agentickit.Unmarshal(data, digest)
			if err != nil {
				t.Fatal(err)
			}
			directory := ".claude"
			if selected == apiv1.HarnessCodex {
				directory = ".agents"
			}
			adapter := &harness.FakeAdapter{Act: func(_ context.Context, req harness.RunRequest) error {
				got, err := os.ReadFile(filepath.Join(req.Workspace, directory, "skills", "implement", "SKILL.md"))
				if err != nil {
					return err
				}
				if string(got) != captured {
					return fmt.Errorf("pod lost captured skill content")
				}
				return harness.WriteCompletion(req.Workspace, req.CompletionPath, apiv1.ResultEnvelope{Status: apiv1.ResultSuccess})
			}}
			registry := harness.NewRegistry()
			if err := registry.RegisterAs(string(selected), adapter); err != nil {
				t.Fatal(err)
			}
			resolver, err := credentials.NewResolver(nil)
			if err != nil {
				t.Fatal(err)
			}
			input := podAgenticExecutorInput(podExecutorWiring{Kit: kit, RunsDir: t.TempDir(), Stderr: io.Discard, Registry: journal.NewRegistryScrubber(), Resolver: resolver, AdapterRegistry: registry})
			executor, err := buildAgenticExecutor(input)
			if err != nil {
				t.Fatal(err)
			}
			env.Workspace = t.TempDir()
			if _, err := executor.Invoke(t.Context(), env); err != nil {
				t.Fatal(err)
			}
		})
	}
}
