package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/fanout"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	wf "github.com/goobers/goobers/internal/workflow"
	"github.com/goobers/goobers/internal/worktree"
)

func TestParityRowMapConformance(t *testing.T) {
	oversize := normalizedOversizeRoster(t)
	htmlEdge := htmlEdgeRoster()
	for _, tc := range []struct {
		name       string
		document   string
		policy     apiv1.BranchFailurePolicy
		onFailure  string
		statuses   map[string]apiv1.ResultStatus
		sourceFail bool
	}{
		{name: "ordered continue on error", document: mapParityRoster(), policy: apiv1.BranchContinueOnError,
			statuses: map[string]apiv1.ResultStatus{"b": apiv1.ResultFailure}},
		{name: "fail fast", document: mapParityRoster(), policy: apiv1.BranchFailFast, onFailure: "fallback",
			statuses: map[string]apiv1.ResultStatus{"a": apiv1.ResultFailure}},
		{name: "empty", document: `{"schemaVersion":1,"items":[]}`, policy: apiv1.BranchContinueOnError},
		{name: "html escaping edge", document: htmlEdge, policy: apiv1.BranchContinueOnError},
		{name: "source invalid onFailure", document: `{"schemaVersion":1,"items":[{"identity":"a","item":{}},{"identity":"a","item":{}}]}`,
			policy: apiv1.BranchFailFast, onFailure: "fallback", sourceFail: true},
		{name: "source invalid abort", document: `{"schemaVersion":1,"items":[{"identity":"a","item":{}},{"identity":"a","item":{}}]}`,
			policy: apiv1.BranchFailFast, onFailure: wf.TargetAbort, sourceFail: true},
		{name: "normalized oversize", document: oversize, policy: apiv1.BranchFailFast,
			onFailure: wf.TargetAbort, sourceFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, scrubber := journal.DefaultScrubber()
			document, err := artifactset.NewSanitizer(scrubber)("application/json", []byte(tc.document))
			if err != nil {
				t.Fatal(err)
			}

			in, acts, _ := staticMapFixtureWith(
				t, string(document), apiv1.IntegrityDerived, tc.statuses,
			)
			in.Run.WorkflowName = "map-parity"
			mapped := &in.Run.Spec.Maps[0]
			mapped.FailurePolicy, mapped.OnFailure, mapped.MaxConcurrent = tc.policy, tc.onFailure, 1
			if tc.onFailure == "fallback" {
				fallback := detTask("fallback", "")
				in.Run.Spec.Tasks = append(in.Run.Spec.Tasks, fallback)
			}
			local := runLocalMapParity(t, in.Run, document, tc.statuses)
			engine := runEngineMapParity(t, in, acts)
			if err := diffConformanceViews(mapConformanceTail(local), mapConformanceTail(engine)); err != nil {
				t.Fatalf("map conformance: %v", err)
			}
			assertMapParityPremise(t, local, tc.sourceFail)
		})
	}
}

func htmlEdgeRoster() string {
	items := make([]string, 32)
	for i := range items {
		items[i] = fmt.Sprintf(
			`{"identity":"item-%02d","item":{"number":%d,"value":"%s"}}`,
			i, i, strings.Repeat("<", 5_000),
		)
	}
	return `{"schemaVersion":1,"items":[` + strings.Join(items, ",") + `]}`
}

func mapParityRoster() string {
	return `{"schemaVersion":1,"items":[` +
		`{"identity":"b","item":{"number":2}},` +
		`{"identity":"a","item":{"number":1}}]}`
}

func normalizedOversizeRoster(t *testing.T) string {
	t.Helper()
	const overhead = len(`{"schemaVersion":1,"items":[{"identity":"a","item":{"value":""}}]}`)
	return `{"schemaVersion":1,"items":[{"identity":"a","item":{"value":"` +
		strings.Repeat("x", int(fanout.MaxSourceBytes)-overhead) + `"}}]}`
}

type mapParityExecutor struct {
	rec      runner.ArtifactRecorder
	document []byte
	statuses map[string]apiv1.ResultStatus
}

func (e *mapParityExecutor) Run(
	ctx context.Context,
	env apiv1.InvocationEnvelope,
	_ apiv1.DeterministicRun,
) (apiv1.ResultEnvelope, error) {
	stage := stageOfTaskID(env.TaskID)
	if stage == "seed" {
		return e.publish(ctx, env)
	}
	if stage == "alpha" {
		identity, _ := env.Inputs["identity"].(string)
		status := e.statuses[identity]
		if status == "" {
			status = apiv1.ResultSuccess
		}
		var failure *apiv1.ErrorInfo
		if status == apiv1.ResultFailure {
			failure = &apiv1.ErrorInfo{Code: "mapped_failed", Message: identity + " failed"}
		}
		return apiv1.ResultEnvelope{
			Status: status, Outputs: map[string]any{"seen": identity},
			Error: failure, Integrity: apiv1.IntegrityDerived,
		}, nil
	}
	return apiv1.ResultEnvelope{
		Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": stage},
		Integrity: apiv1.IntegrityDerived,
	}, nil
}

func (e *mapParityExecutor) publish(
	ctx context.Context,
	env apiv1.InvocationEnvelope,
) (apiv1.ResultEnvelope, error) {
	dir, err := os.MkdirTemp("", "map-parity-*")
	if err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := os.WriteFile(filepath.Join(dir, "roster.json"), e.document, 0o600); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	manifest, err := json.Marshal(artifactset.Manifest{
		SchemaVersion: artifactset.SchemaVersion,
		Entries: []artifactset.ManifestEntry{{
			Name: "roster", Path: "roster.json", MediaType: "application/json",
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
		Status: apiv1.ResultSuccess, Outputs: map[string]any{"value": stageOfTaskID(env.TaskID)},
		Artifacts: pointers, Integrity: apiv1.IntegrityDerived,
	}, nil
}

func runLocalMapParity(
	t *testing.T,
	in RunInput,
	document []byte,
	statuses map[string]apiv1.ResultStatus,
) []journal.Event {
	t.Helper()
	root := t.TempDir()
	worktrees, err := worktree.NewManager(filepath.Join(root, "workcopies"))
	if err != nil {
		t.Fatal(err)
	}
	runsDir := filepath.Join(root, "runs")
	local, err := runner.New(runner.Config{
		NewDeterministic: func(rec runner.ArtifactRecorder, _ runner.SecretRegistrar) (invoke.Deterministic, error) {
			return &mapParityExecutor{rec: rec, document: document, statuses: statuses}, nil
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

func runEngineMapParity(
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
		t.Fatalf("engine map parity: %+v", out)
	}
	return liveEvents(t, dir, in.Run.RunID)
}

func mapConformanceTail(events []journal.Event) []journal.Event {
	for i := range events {
		event := events[i]
		if event.Type == journal.EventMapStarted ||
			(event.Type == journal.EventError && event.Parallel == "items") {
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

func assertMapParityPremise(t *testing.T, events []journal.Event, sourceFail bool) {
	t.Helper()
	for i := range events {
		event := events[i]
		if sourceFail && event.Type == journal.EventError && event.Parallel == "items" &&
			event.Error != nil && event.Error.Code == runner.FanOutSourceInvalidCode {
			return
		}
		if !sourceFail && event.Type == journal.EventMapStarted {
			if event.MapRoster == nil || event.MapRoster.RosterDigest == "" {
				t.Fatalf("runner map.started has no frozen roster digest: %+v", event)
			}
			return
		}
	}
	if sourceFail {
		t.Fatal("runner did not journal fanout_source_invalid")
	}
	t.Fatal("runner did not journal map.started")
}
