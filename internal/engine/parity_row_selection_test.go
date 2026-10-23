package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func TestParityRowSelectiveParallelConformance(t *testing.T) {
	for _, tc := range []struct {
		name        string
		document    string
		allOptional bool
		sourceFail  bool
		onFailure   string
	}{
		{name: "selected and skipped", document: `{"schemaVersion":1,"branches":[{"name":"gamma","reason":"affected"}]}`},
		{name: "all optional skipped", document: `{"schemaVersion":1,"branches":[]}`, allOptional: true},
		{name: "source invalid onFailure", document: `{"schemaVersion":1,"branches":[{"name":"missing"}]}`, sourceFail: true, onFailure: "fallback"},
		{name: "source invalid abort", document: `{"schemaVersion":1,"branches":[{"name":"missing"}]}`, sourceFail: true, onFailure: wf.TargetAbort},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, scrubber := journal.DefaultScrubber()
			document, err := artifactset.NewSanitizer(scrubber)("application/json", []byte(tc.document))
			if err != nil {
				t.Fatal(err)
			}
			in, acts, _ := selectiveParallelFixtureWithIntegrity(
				t, string(document), apiv1.IntegrityDerived,
			)
			in.Run.WorkflowName = "selection-parity"
			in.Run.Spec.Parallels[0].MaxConcurrentBranches = 1
			if tc.allOptional {
				in.Run.Spec.Parallels[0].Branches[0].Optional = true
			}
			if tc.sourceFail {
				in.Run.Spec.Parallels[0].FailurePolicy = apiv1.BranchFailFast
				in.Run.Spec.Parallels[0].OnFailure = tc.onFailure
				if tc.onFailure == "fallback" {
					fallback := detTask(tc.onFailure, "")
					in.Run.Spec.Tasks = append(in.Run.Spec.Tasks, fallback)
				}
			}
			local := runLocalSelectionParity(t, in.Run, document)
			engine := runEngineSelectionParity(t, in, acts)
			if err := diffConformanceViews(
				selectionConformanceTail(local),
				selectionConformanceTail(engine),
			); err != nil {
				t.Fatalf("selection conformance: %v", err)
			}

			assertSelectionParityPremise(t, local, tc.allOptional, tc.sourceFail, tc.onFailure)
		})
	}
}

func selectionConformanceTail(events []journal.Event) []journal.Event {
	for i := range events {
		event := events[i]
		if event.Type == journal.EventParallelStarted ||
			(event.Type == journal.EventError && event.Parallel == "parallel") {
			out := append([]journal.Event(nil), events[i:]...)
			for j := range out {
				out[j].Seq = uint64(j + 1)
				out[j].Disposition = ""
			}
			return out
		}
	}
	return nil
}

type selectionParityExecutor struct {
	rec      runner.ArtifactRecorder
	document []byte
}

func (e *selectionParityExecutor) Run(
	ctx context.Context,
	env apiv1.InvocationEnvelope,
	_ apiv1.DeterministicRun,
) (apiv1.ResultEnvelope, error) {
	stage := stageOfTaskID(env.TaskID)
	if stage != "seed" {
		return apiv1.ResultEnvelope{
			Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": stage},
		}, nil
	}
	dir, err := os.MkdirTemp("", "selection-parity-*")
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "selection.json"), e.document, 0o600); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	manifest, err := json.Marshal(artifactset.Manifest{
		SchemaVersion: artifactset.SchemaVersion,
		Entries: []artifactset.ManifestEntry{{
			Name: "selection", Path: "selection.json", MediaType: "application/json",
		}},
	})
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o600); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	_, scrubber := journal.DefaultScrubber()
	prepared, err := artifactset.Prepare(ctx, dir, "manifest.json", artifactset.NewSanitizer(scrubber))
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	if err := prepared.Bind(env.ArtifactPublication, env.Attempt); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	pointers, err := prepared.Publish(ctx, func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
		ref, err := e.rec.RecordArtifact(env.TaskID+"/"+name, data)
		return apiv1.ArtifactPointer{
			Path: ref.Path, Digest: ref.Digest, MediaType: media,
			Size: ref.Size, Integrity: ref.Integrity,
		}, err
	})
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	return apiv1.ResultEnvelope{
		Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": stage},
		Artifacts: pointers,
	}, nil
}

func runLocalSelectionParity(t *testing.T, in RunInput, document []byte) []journal.Event {
	t.Helper()
	root := t.TempDir()
	worktrees, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	runsDir := filepath.Join(root, "runs")
	local, err := runner.New(runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, _ runner.SecretRegistrar) (invoke.Deterministic, error) {
			return &selectionParityExecutor{rec: rec, document: document}, nil
		},
		Worktrees: worktrees, RunsDir: runsDir, ScratchDir: filepath.Join(root, "scratch"),
	})
	if err != nil {
		t.Fatal(err)
	}
	machine, err := wf.Compile(wf.Definition{
		Name: in.WorkflowName, Version: in.Version, DSLVersion: in.DSLVersion, Spec: in.Spec,
	}, wf.WithPreviewFeatures(true))
	if err != nil {
		t.Fatal(err)
	}
	runID := "runner-" + strings.ReplaceAll(t.Name(), "/", "-")
	if _, err := local.Start(context.Background(), runner.StartInput{
		RunID: runID, Machine: machine, Gaggle: in.Gaggle,
		Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}); err != nil {
		t.Fatal(err)
	}
	return readJournalEvents(t, filepath.Join(runsDir, runID))
}

func runEngineSelectionParity(
	t *testing.T,
	in staticSchedulerInput,
	acts *Activities,
) []journal.Event {
	t.Helper()
	in.Run.RunID = "engine-" + strings.ReplaceAll(t.Name(), "/", "-")
	writer, dir := newLiveWriter(t)
	acts.Journal = writer
	out := executeScheduler(t, in, acts)
	if out.Error != "" {
		t.Fatalf("engine selection parity: %+v", out)
	}
	return liveEvents(t, dir, in.Run.RunID)
}

func assertSelectionParityPremise(
	t *testing.T,
	events []journal.Event,
	allOptional, sourceFail bool,
	onFailure string,
) {
	t.Helper()
	var started *journal.Event
	for i := range events {
		event := &events[i]
		if event.Type == journal.EventParallelStarted {
			started = event
		}
	}
	if sourceFail {
		if started != nil {
			t.Fatal("runner recorded parallel.started after an invalid selection source")
		}
		found := false
		for _, event := range events {
			found = found || (event.Type == journal.EventError && event.Parallel == "parallel" &&
				event.Error != nil && event.Error.Code == runner.FanOutSourceInvalidCode)
		}
		if !found {
			t.Fatal("runner did not journal fanout_source_invalid")
		}
		return
	}
	if started == nil || started.Selection == nil {
		t.Fatal("runner did not freeze selection on parallel.started")
	}
	skipped := 0
	for _, outcome := range started.Completeness {
		if outcome.Status == journal.BranchSkipped {
			skipped++
			if outcome.Reason != "not selected by seed.selection" {
				t.Fatalf("runner skip reason = %q", outcome.Reason)
			}
		}
	}
	if allOptional && skipped != len(started.Completeness) {
		t.Fatalf("runner skipped %d/%d optional branches", skipped, len(started.Completeness))
	}
	_ = onFailure
}
