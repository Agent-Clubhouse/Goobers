package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

type tutorHoldoutPollerStub struct {
	result providers.PullRequestPollResult
	err    error
}

func (s tutorHoldoutPollerStub) PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error) {
	return s.result, s.err
}

func TestTutorChangeClassificationRequiresLiveVerification(t *testing.T) {
	tests := []struct {
		name  string
		types []tutorChangeType
		want  bool
	}{
		{name: "persona optional", types: []tutorChangeType{tutorChangePersona}},
		{name: "gate tune optional", types: []tutorChangeType{tutorChangeGateTune}},
		{name: "structure required", types: []tutorChangeType{tutorChangeStructure}, want: true},
		{name: "skill required", types: []tutorChangeType{tutorChangeSkill}, want: true},
		{name: "validation required", types: []tutorChangeType{tutorChangeValidation}, want: true},
		{name: "mixed risk required", types: []tutorChangeType{tutorChangePersona, tutorChangeStructure}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			classification := tutorChangeClassification{Types: tc.types}
			if got := classification.RequiresLiveVerification(); got != tc.want {
				t.Fatalf("RequiresLiveVerification() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPrepareTutorHoldoutPinsPreAndPostVersionAxes(t *testing.T) {
	root := initDemo(t)
	const (
		gaggle = "example"
		runID  = "tutor-authoring"
	)
	liveConfig := instance.NewLayout(root).ConfigDir()
	proposedConfig := filepath.Join(t.TempDir(), "proposed")
	if err := os.CopyFS(proposedConfig, os.DirFS(liveConfig)); err != nil {
		t.Fatalf("copy config: %v", err)
	}

	workflowPath := filepath.Join(proposedConfig, "gaggles", gaggle, "workflows", "default-implement.yaml")
	before, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(before, &workflow); err != nil {
		t.Fatal(err)
	}
	workflow.Spec.DisplayName += " changed"
	after, err := yaml.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, after, 0o644); err != nil {
		t.Fatal(err)
	}

	run, err := journal.Create(instance.NewLayout(root).ForGaggle(gaggle).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "tutor", Gaggle: gaggle,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	findingRef, err := run.RecordStageArtifact("analyze", 1, "", "finding.md", []byte("recurring failure"))
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	record, err := prepareTutorHoldout(
		root,
		gaggle,
		runID,
		proposedConfig,
		tutorChangeClassification{Types: []tutorChangeType{tutorChangeStructure}},
		[]tutorFileChange{{Path: filepath.ToSlash(workflowPath), Before: before, After: after}},
		time.Now(),
	)
	if err != nil {
		t.Fatalf("prepareTutorHoldout: %v", err)
	}
	if record == nil || record.FindingDigest != findingRef.Digest {
		t.Fatalf("record = %+v, want finding digest %s", record, findingRef.Digest)
	}
	if len(record.Targets) != 1 || record.Targets[0].Workflow != "default-implement" {
		t.Fatalf("targets = %+v, want default-implement", record.Targets)
	}
	if record.Targets[0].OldAxes == record.Targets[0].NewAxes {
		t.Fatalf("old/new axes must identify a real transition: %+v", record.Targets[0])
	}
	if record.State != tutorHoldoutStatePending {
		t.Fatalf("state = %q, want pending", record.State)
	}
}

func TestPrepareTutorHoldoutPinsSkillBodyTransition(t *testing.T) {
	root := initDemo(t)
	const (
		gaggle = "example"
		runID  = "tutor-skill-authoring"
	)
	liveConfig := instance.NewLayout(root).ConfigDir()
	liveSkillPath := filepath.Join(root, "skills", "implement", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(liveSkillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	before := []byte("# Implement\n\nUse the original approach.\n")
	if err := os.WriteFile(liveSkillPath, before, 0o644); err != nil {
		t.Fatal(err)
	}

	proposedRoot := t.TempDir()
	proposedConfig := filepath.Join(proposedRoot, "proposed")
	if err := os.CopyFS(proposedConfig, os.DirFS(liveConfig)); err != nil {
		t.Fatalf("copy config: %v", err)
	}
	proposedSkillPath := filepath.Join(proposedRoot, "skills", "implement", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(proposedSkillPath), 0o755); err != nil {
		t.Fatal(err)
	}
	after := []byte("# Implement\n\nUse the improved approach.\n")
	if err := os.WriteFile(proposedSkillPath, after, 0o644); err != nil {
		t.Fatal(err)
	}

	run, err := journal.Create(instance.NewLayout(root).ForGaggle(gaggle).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "tutor", Gaggle: gaggle,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordStageArtifact("analyze", 1, "", "finding.md", []byte("skill gap")); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}

	record, err := prepareTutorHoldout(
		root,
		gaggle,
		runID,
		proposedConfig,
		tutorChangeClassification{Types: []tutorChangeType{tutorChangeSkill}},
		[]tutorFileChange{{Path: filepath.ToSlash(proposedSkillPath), Before: before, After: after}},
		time.Now(),
	)
	if err != nil {
		t.Fatalf("prepareTutorHoldout: %v", err)
	}
	if record == nil || len(record.Targets) != 1 || record.Targets[0].Workflow != "default-implement" {
		t.Fatalf("record = %+v, want default-implement skill holdout", record)
	}
	target := record.Targets[0]
	if target.OldAxes.WorkflowDigest != target.NewAxes.WorkflowDigest {
		t.Fatalf("skill-only change moved workflow digest: %+v", target)
	}
	if target.OldAxes.GooberDigest == target.NewAxes.GooberDigest {
		t.Fatalf("skill-only change did not move goober digest: %+v", target)
	}
}

func TestPrepareTutorHoldoutSupportsWorkflowLifecycleChanges(t *testing.T) {
	root := initDemo(t)
	const gaggle = "example"
	liveConfig := instance.NewLayout(root).ConfigDir()
	workflowPath := filepath.Join(liveConfig, "gaggles", gaggle, "workflows", "default-implement.yaml")
	workflowRaw, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(workflowRaw, &workflow); err != nil {
		t.Fatal(err)
	}
	retired := workflow
	retired.Name = "retired-implement"
	retiredRaw, err := yaml.Marshal(retired)
	if err != nil {
		t.Fatal(err)
	}
	retiredPath := filepath.Join(liveConfig, "gaggles", gaggle, "workflows", "retired-implement.yaml")
	if err := os.WriteFile(retiredPath, retiredRaw, 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name      string
		lifecycle tutorHoldoutLifecycle
		mutate    func(t *testing.T, proposed string) tutorFileChange
	}{
		{
			name: "addition", lifecycle: tutorHoldoutAddition,
			mutate: func(t *testing.T, proposed string) tutorFileChange {
				t.Helper()
				added := workflow
				added.Name = "added-implement"
				raw, err := yaml.Marshal(added)
				if err != nil {
					t.Fatal(err)
				}
				addedPath := filepath.Join(proposed, "gaggles", gaggle, "workflows", "added-implement.yaml")
				if err := os.WriteFile(addedPath, raw, 0o644); err != nil {
					t.Fatal(err)
				}
				return tutorFileChange{Path: filepath.ToSlash(addedPath), After: raw}
			},
		},
		{
			name: "removal", lifecycle: tutorHoldoutRemoval,
			mutate: func(t *testing.T, proposed string) tutorFileChange {
				t.Helper()
				removedPath := filepath.Join(proposed, "gaggles", gaggle, "workflows", "retired-implement.yaml")
				if err := os.Remove(removedPath); err != nil {
					t.Fatal(err)
				}
				return tutorFileChange{Path: filepath.ToSlash(removedPath), Before: retiredRaw}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proposed := filepath.Join(t.TempDir(), "proposed")
			if err := os.CopyFS(proposed, os.DirFS(liveConfig)); err != nil {
				t.Fatal(err)
			}
			change := tc.mutate(t, proposed)
			runID := "tutor-lifecycle-" + tc.name
			writeTutorFindingFixture(t, root, gaggle, runID)
			record, err := prepareTutorHoldout(
				root,
				gaggle,
				runID,
				proposed,
				tutorChangeClassification{Types: []tutorChangeType{tutorChangeStructure}},
				[]tutorFileChange{change},
				time.Now(),
			)
			if err != nil {
				t.Fatal(err)
			}
			if record == nil || len(record.Targets) != 1 {
				t.Fatalf("record = %+v, want one lifecycle target", record)
			}
			target := record.Targets[0]
			if target.Lifecycle != tc.lifecycle {
				t.Fatalf("lifecycle = %q, want %q", target.Lifecycle, tc.lifecycle)
			}
			if tc.lifecycle == tutorHoldoutAddition && target.OldAxes != (tutorVersionAxes{}) {
				t.Fatalf("addition old axes = %+v, want empty", target.OldAxes)
			}
			if tc.lifecycle == tutorHoldoutRemoval && target.NewAxes != (tutorVersionAxes{}) {
				t.Fatalf("removal new axes = %+v, want empty", target.NewAxes)
			}
		})
	}
}

func TestRefreshTutorHoldoutMergeStatePinsMergedPR(t *testing.T) {
	root := initDemo(t)
	createdAt := time.Now().UTC().Add(-time.Hour)
	record := tutorHoldoutRecord{
		Schema: tutorHoldoutSchemaVersion, ID: "sha256:merge", FindingDigest: "sha256:finding",
		Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42,
		State: tutorHoldoutStatePending, CreatedAt: createdAt,
	}
	if err := writeTutorHoldout(root, record); err != nil {
		t.Fatal(err)
	}
	mergedAt := time.Now().UTC()
	err := refreshTutorHoldoutMergeState(
		context.Background(),
		root,
		"example",
		providers.RepositoryRef{Owner: "acme", Name: "app"},
		tutorHoldoutPollerStub{result: providers.PullRequestPollResult{
			Number: 42, Merged: true, MergedAt: &mergedAt,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	records, err := loadTutorHoldouts(root, "example")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].MergedAt == nil || !records[0].MergedAt.Equal(mergedAt) {
		t.Fatalf("records = %+v, want merged timestamp %s", records, mergedAt)
	}
}

func TestPrepareTutorHoldoutSkipsOptionalPersonaChange(t *testing.T) {
	record, err := prepareTutorHoldout(
		"", "", "", "",
		tutorChangeClassification{Types: []tutorChangeType{tutorChangePersona}},
		nil,
		time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}

	if record != nil {
		t.Fatalf("record = %+v, want nil for optional persona holdout", record)
	}
}

func TestClearTutorHoldoutsForRunReplacesRepassState(t *testing.T) {
	root := initDemo(t)
	if name := filepath.Base(instance.NewLayout(root).TutorHoldoutPath("example", "sha256:finding")); strings.Contains(name, ":") {
		t.Fatalf("Tutor holdout filename %q is not portable", name)
	}
	for _, record := range []tutorHoldoutRecord{
		{
			Schema: tutorHoldoutSchemaVersion, ID: "sha256:first", FindingDigest: "sha256:finding-1",
			Gaggle: "example", AuthoringRunID: "same-run", State: tutorHoldoutStatePending, CreatedAt: time.Now(),
		},
		{
			Schema: tutorHoldoutSchemaVersion, ID: "sha256:other", FindingDigest: "sha256:finding-2",
			Gaggle: "example", AuthoringRunID: "other-run", State: tutorHoldoutStatePending, CreatedAt: time.Now(),
		},
	} {
		if err := writeTutorHoldout(root, record); err != nil {
			t.Fatal(err)
		}
	}
	replacement := tutorHoldoutRecord{
		Schema: tutorHoldoutSchemaVersion, ID: "sha256:replacement", FindingDigest: "sha256:replacement",
		Gaggle: "example", AuthoringRunID: "same-run", State: tutorHoldoutStatePending, CreatedAt: time.Now(),
	}
	if err := writeTutorHoldout(root, replacement); err != nil {
		t.Fatal(err)
	}
	records, err := loadTutorHoldouts(root, "example")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records after atomic same-run replacement = %+v, want two run-keyed records", records)
	}
	for _, record := range records {
		if record.AuthoringRunID == "same-run" && record.ID != replacement.ID {
			t.Fatalf("same-run record = %+v, want replacement", record)
		}
	}
	if err := clearTutorHoldoutsForRun(root, "example", "same-run"); err != nil {
		t.Fatal(err)
	}
	records, err = loadTutorHoldouts(root, "example")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].AuthoringRunID != "other-run" {
		t.Fatalf("records = %+v, want only the unrelated run", records)
	}
}

func TestVerifyTutorHoldoutUsesPinnedTransitionNotLatest(t *testing.T) {
	root := initDemo(t)
	createdAt := time.Now().UTC().Add(-time.Hour)
	record := tutorHoldoutRecord{
		Schema:         tutorHoldoutSchemaVersion,
		ID:             "sha256:pinned",
		FindingDigest:  "sha256:finding",
		Gaggle:         "example",
		AuthoringRunID: "authoring",
		ChangeTypes:    []tutorChangeType{tutorChangeStructure},
		State:          tutorHoldoutStatePending,
		CreatedAt:      createdAt,
		Targets: []tutorHoldoutTarget{{
			Workflow: "implementation",
			OldAxes:  tutorVersionAxes{WorkflowDigest: "sha256:old", GooberDigest: "sha256:goober-old"},
			NewAxes:  tutorVersionAxes{WorkflowDigest: "sha256:new", GooberDigest: "sha256:goober-new"},
		}},
	}
	if err := writeTutorHoldout(root, record); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "old-"+string(rune('a'+i)), "implementation",
			"sha256:old", "sha256:goober-old", "model-a", "1.0.0", journal.PhaseFailed)
	}
	// Same workflow name in another gaggle must not break this gaggle's
	// old->new transition or contribute efficacy samples.
	writeEffectiveVersionFixtureRunForGaggle(
		t, root, "other", "foreign", "implementation",
		"sha256:foreign", "sha256:foreign-goober", "foreign-model", "9.0.0", journal.PhaseCompleted,
	)
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "new-"+string(rune('a'+i)), "implementation",
			"sha256:new", "sha256:goober-new", "model-a", "1.0.0", journal.PhaseCompleted)
	}

	// A later unrelated transition regresses. A latest-transition check would
	// assess this cohort and leave the wrong finding open.
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "later-"+string(rune('a'+i)), "implementation",
			"sha256:later", "sha256:goober-later", "model-b", "2.0.0", journal.PhaseFailed)
	}
	rebuildTelemetryQueryRollup(t, root)
	db, err := openRollup(instance.NewLayout(root), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	thresholds := rollup.DefaultEfficacyThresholds()
	prepared, err := verifyTutorHoldouts(
		root, "example", db, 24*time.Hour, time.Now().Add(-24*time.Hour), time.Now(), thresholds,
	)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.CanProceed ||
		prepared.Findings[0].Targets[0].VerificationNote != "authoring pull request has not been finalized" {
		t.Fatalf("prepared result = %+v, want fail-closed pending state", prepared)
	}
	record.PRNumber = 42
	record.MergedAt = &createdAt
	if err := writeTutorHoldout(root, record); err != nil {
		t.Fatal(err)
	}
	result, err := verifyTutorHoldouts(
		root, "example", db, 24*time.Hour, time.Now().Add(-24*time.Hour), time.Now(), thresholds,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CanProceed || result.PendingCount != 0 {
		t.Fatalf("result = %+v, want closed holdout", result)
	}
	if len(result.Findings) != 1 || result.Findings[0].State != tutorHoldoutStateClosedHelped {
		t.Fatalf("findings = %+v, want closed-helped", result.Findings)
	}
	target := result.Findings[0].Targets[0]
	if target.NewVersion == nil || target.NewVersion.WorkflowDigest != "sha256:new" || target.Verdict != rollup.EfficacyHelped {
		t.Fatalf("target = %+v, want pinned new cohort with helped verdict", target)
	}
}

func TestVerifyTutorHoldoutRefreshesAmendedTransitionFromPostMergeTelemetry(t *testing.T) {
	root := initDemo(t)
	const workflowName = "default-implement"
	finalConfig := filepath.Join(t.TempDir(), "final")
	if err := os.CopyFS(finalConfig, os.DirFS(instance.NewLayout(root).ConfigDir())); err != nil {
		t.Fatal(err)
	}
	workflowPath := filepath.Join(finalConfig, "gaggles", "example", "workflows", workflowName+".yaml")
	raw, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatal(err)
	}
	var definition apiv1.Workflow
	if err := yaml.Unmarshal(raw, &definition); err != nil {
		t.Fatal(err)
	}
	definition.Spec.DisplayName = "final amended behavior"
	raw, err = yaml.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(workflowPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	finalVersions, err := tutorConfigVersions(finalConfig, "example", []string{workflowName})
	if err != nil {
		t.Fatal(err)
	}
	finalAxes := finalVersions[workflowName]
	proposalAxes := tutorVersionAxes{WorkflowDigest: "sha256:proposal", GooberDigest: finalAxes.GooberDigest}
	interveningAxes := tutorVersionAxes{WorkflowDigest: "sha256:intervening", GooberDigest: finalAxes.GooberDigest}
	createdAt := time.Now().UTC()
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(
			t, root, "initial-"+string(rune('a'+i)), workflowName,
			"sha256:initial", finalAxes.GooberDigest, "model", "1.0.0", journal.PhaseFailed,
		)
	}
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(
			t, root, "intervening-"+string(rune('a'+i)), workflowName,
			interveningAxes.WorkflowDigest, interveningAxes.GooberDigest, "model", "1.0.0", journal.PhaseFailed,
		)
	}
	mergedAt := time.Now().UTC()
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(
			t, root, "final-"+string(rune('a'+i)), workflowName,
			finalAxes.WorkflowDigest, finalAxes.GooberDigest, "model", "1.0.0", journal.PhaseCompleted,
		)
	}
	record := tutorHoldoutRecord{
		Schema: tutorHoldoutSchemaVersion, ID: "sha256:amended", FindingDigest: "sha256:finding",
		Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42,
		MergedAt: &mergedAt, ChangeTypes: []tutorChangeType{tutorChangeStructure},
		State: tutorHoldoutStatePending, CreatedAt: createdAt,
		Targets: []tutorHoldoutTarget{{
			Workflow: workflowName, Lifecycle: tutorHoldoutTransition,
			OldAxes: tutorVersionAxes{WorkflowDigest: "sha256:initial", GooberDigest: finalAxes.GooberDigest},
			NewAxes: proposalAxes,
		}},
	}
	if err := writeTutorHoldout(root, record); err != nil {
		t.Fatal(err)
	}
	rebuildTelemetryQueryRollup(t, root)
	db, err := openRollup(instance.NewLayout(root), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	result, err := verifyTutorHoldouts(
		root, "example", db, 24*time.Hour, createdAt.Add(-24*time.Hour), time.Now(),
		rollup.DefaultEfficacyThresholds(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CanProceed {
		t.Fatalf("result = %+v, want amended final transition to close", result)
	}
	target := result.Findings[0].Targets[0]
	if target.OldAxes != interveningAxes || target.NewAxes != finalAxes {
		t.Fatalf("refreshed axes = %+v -> %+v, want %+v -> %+v", target.OldAxes, target.NewAxes, interveningAxes, finalAxes)
	}
	if target.NewAxes == proposalAxes {
		t.Fatalf("new axes retained stale proposal value: %+v", target.NewAxes)
	}
}

func TestVerifyTutorHoldoutWorkflowLifecycleSemantics(t *testing.T) {
	t.Run("addition requires healthy post-promotion cohort", func(t *testing.T) {
		root := initDemo(t)
		mergedAt := time.Now().UTC()
		axes := tutorVersionAxes{WorkflowDigest: "sha256:added", GooberDigest: "sha256:goober"}
		record := tutorHoldoutRecord{
			Schema: tutorHoldoutSchemaVersion, ID: "sha256:addition", FindingDigest: "sha256:finding",
			Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42, MergedAt: &mergedAt,
			State: tutorHoldoutStatePending, CreatedAt: mergedAt.Add(-time.Hour),
			Targets: []tutorHoldoutTarget{{
				Workflow: "added-workflow", Lifecycle: tutorHoldoutAddition, NewAxes: axes,
			}},
		}
		if err := writeTutorHoldout(root, record); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			writeEffectiveVersionFixtureRun(
				t, root, "added-"+string(rune('a'+i)), "added-workflow",
				axes.WorkflowDigest, axes.GooberDigest, "model", "1.0.0", journal.PhaseCompleted,
			)
		}
		rebuildTelemetryQueryRollup(t, root)
		db, err := openRollup(instance.NewLayout(root), false)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		result, err := verifyTutorHoldouts(
			root, "example", db, 24*time.Hour, mergedAt.Add(-24*time.Hour), time.Now(),
			rollup.DefaultEfficacyThresholds(),
		)
		if err != nil {
			t.Fatal(err)
		}
		if !result.CanProceed || result.Findings[0].Targets[0].After.CompletedRuns != 5 {
			t.Fatalf("result = %+v, want healthy added cohort to close", result)
		}
	})

	t.Run("removal waits for live reconciliation", func(t *testing.T) {
		root := initDemo(t)
		const workflowName = "retired-implement"
		liveConfig := instance.NewLayout(root).ConfigDir()
		sourcePath := filepath.Join(liveConfig, "gaggles", "example", "workflows", "default-implement.yaml")
		raw, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		var definition apiv1.Workflow
		if err := yaml.Unmarshal(raw, &definition); err != nil {
			t.Fatal(err)
		}
		definition.Name = workflowName
		raw, err = yaml.Marshal(definition)
		if err != nil {
			t.Fatal(err)
		}
		liveWorkflow := filepath.Join(liveConfig, "gaggles", "example", "workflows", workflowName+".yaml")
		if err := os.WriteFile(liveWorkflow, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		finalConfig := filepath.Join(t.TempDir(), "final")
		if err := os.CopyFS(finalConfig, os.DirFS(liveConfig)); err != nil {
			t.Fatal(err)
		}
		finalWorkflow := filepath.Join(finalConfig, "gaggles", "example", "workflows", workflowName+".yaml")
		if err := os.Remove(finalWorkflow); err != nil {
			t.Fatal(err)
		}
		mergedAt := time.Now().UTC()
		record := tutorHoldoutRecord{
			Schema: tutorHoldoutSchemaVersion, ID: "sha256:removal", FindingDigest: "sha256:finding",
			Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42,
			MergedAt: &mergedAt, State: tutorHoldoutStatePending, CreatedAt: mergedAt.Add(-time.Hour),
			Targets: []tutorHoldoutTarget{{
				Workflow: workflowName, Lifecycle: tutorHoldoutRemoval,
				OldAxes: tutorVersionAxes{WorkflowDigest: "sha256:old", GooberDigest: "sha256:goober"},
			}},
		}
		if err := writeTutorHoldout(root, record); err != nil {
			t.Fatal(err)
		}
		pending, err := verifyTutorHoldouts(
			root, "example", nil, 24*time.Hour, mergedAt.Add(-24*time.Hour), time.Now(),
			rollup.DefaultEfficacyThresholds(),
		)
		if err != nil {
			t.Fatal(err)
		}
		if pending.CanProceed {
			t.Fatalf("result = %+v, want removal pending while live config still contains workflow", pending)
		}
		if err := os.Remove(liveWorkflow); err != nil {
			t.Fatal(err)
		}
		closed, err := verifyTutorHoldouts(
			root, "example", nil, 24*time.Hour, mergedAt.Add(-24*time.Hour), time.Now(),
			rollup.DefaultEfficacyThresholds(),
		)
		if err != nil {
			t.Fatal(err)
		}
		if !closed.CanProceed || closed.Findings[0].Targets[0].Verdict != rollup.EfficacyHelped {
			t.Fatalf("result = %+v, want reconciled removal to close", closed)
		}
	})
}

func TestVerifyTutorHoldoutExcludesEarlierRunsFromRepromotedCohort(t *testing.T) {
	root := initDemo(t)
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "initial-old-"+string(rune('a'+i)), "implementation",
			"sha256:old", "sha256:goober", "model", "1.0.0", journal.PhaseFailed)
	}

	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "earlier-new-"+string(rune('a'+i)), "implementation",
			"sha256:new", "sha256:goober", "model", "1.0.0", journal.PhaseCompleted)
	}
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "reverted-old-"+string(rune('a'+i)), "implementation",
			"sha256:old", "sha256:goober", "model", "1.0.0", journal.PhaseFailed)
	}

	createdAt := time.Now().UTC()
	record := tutorHoldoutRecord{
		Schema: tutorHoldoutSchemaVersion, ID: "sha256:repromoted", FindingDigest: "sha256:finding",
		Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42, ChangeTypes: []tutorChangeType{tutorChangeStructure},
		State: tutorHoldoutStatePending, CreatedAt: createdAt, MergedAt: &createdAt,
		Targets: []tutorHoldoutTarget{{
			Workflow: "implementation",
			OldAxes:  tutorVersionAxes{WorkflowDigest: "sha256:old", GooberDigest: "sha256:goober"},
			NewAxes:  tutorVersionAxes{WorkflowDigest: "sha256:new", GooberDigest: "sha256:goober"},
		}},
	}
	if err := writeTutorHoldout(root, record); err != nil {
		t.Fatal(err)
	}
	writeEffectiveVersionFixtureRun(t, root, "repromoted-new", "implementation",
		"sha256:new", "sha256:goober", "model", "1.0.0", journal.PhaseCompleted)
	rebuildTelemetryQueryRollup(t, root)
	db, err := openRollup(instance.NewLayout(root), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	result, err := verifyTutorHoldouts(
		root, "example", db, 24*time.Hour, time.Now().Add(-24*time.Hour), time.Now(),
		rollup.DefaultEfficacyThresholds(),
	)
	if err != nil {
		t.Fatal(err)
	}
	target := result.Findings[0].Targets[0]
	if result.CanProceed || target.Verdict != rollup.EfficacyInsufficientData {
		t.Fatalf("result = %+v, want pending until the re-promoted cohort itself has enough samples", result)
	}
	if target.Before.TotalRuns != 5 || target.After.TotalRuns != 1 {
		t.Fatalf("before/after = %d/%d, want contiguous 5 pre-promotion and 1 post-promotion", target.Before.TotalRuns, target.After.TotalRuns)
	}
}

func TestVerifyTutorHoldoutExcludesLaterCohortReentry(t *testing.T) {
	root := initDemo(t)
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "old-"+string(rune('a'+i)), "implementation",
			"sha256:old", "sha256:goober", "model", "1.0.0", journal.PhaseFailed)
	}
	createdAt := time.Now().UTC()
	record := tutorHoldoutRecord{
		Schema: tutorHoldoutSchemaVersion, ID: "sha256:reentry", FindingDigest: "sha256:finding",
		Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42, ChangeTypes: []tutorChangeType{tutorChangeStructure},
		State: tutorHoldoutStatePending, CreatedAt: createdAt, MergedAt: &createdAt,
		Targets: []tutorHoldoutTarget{{
			Workflow: "implementation",
			OldAxes:  tutorVersionAxes{WorkflowDigest: "sha256:old", GooberDigest: "sha256:goober"},
			NewAxes:  tutorVersionAxes{WorkflowDigest: "sha256:new", GooberDigest: "sha256:goober"},
		}},
	}
	if err := writeTutorHoldout(root, record); err != nil {
		t.Fatal(err)
	}
	writeEffectiveVersionFixtureRun(t, root, "first-new", "implementation",
		"sha256:new", "sha256:goober", "model", "1.0.0", journal.PhaseCompleted)
	writeEffectiveVersionFixtureRun(t, root, "intervening", "implementation",
		"sha256:other", "sha256:other-goober", "model", "1.0.0", journal.PhaseFailed)
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "later-new-"+string(rune('a'+i)), "implementation",
			"sha256:new", "sha256:goober", "model", "1.0.0", journal.PhaseCompleted)
	}
	rebuildTelemetryQueryRollup(t, root)
	db, err := openRollup(instance.NewLayout(root), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	result, err := verifyTutorHoldouts(
		root, "example", db, 24*time.Hour, time.Now().Add(-24*time.Hour), time.Now(),
		rollup.DefaultEfficacyThresholds(),
	)
	if err != nil {
		t.Fatal(err)
	}
	target := result.Findings[0].Targets[0]
	if target.After.TotalRuns != 1 || target.Verdict != rollup.EfficacyInsufficientData {
		t.Fatalf("target = %+v, want only the first contiguous new cohort", target)
	}
}

func TestVerifyTutorHoldoutBaselineWindowIsAnchoredToFinding(t *testing.T) {
	root := initDemo(t)
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "old-"+string(rune('a'+i)), "implementation",
			"sha256:old", "sha256:goober", "model", "1.0.0", journal.PhaseFailed)
	}
	createdAt := time.Now().UTC()
	record := tutorHoldoutRecord{
		Schema: tutorHoldoutSchemaVersion, ID: "sha256:delayed", FindingDigest: "sha256:finding",
		Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42, ChangeTypes: []tutorChangeType{tutorChangeStructure},
		State: tutorHoldoutStatePending, CreatedAt: createdAt, MergedAt: &createdAt,
		Targets: []tutorHoldoutTarget{{
			Workflow: "implementation",
			OldAxes:  tutorVersionAxes{WorkflowDigest: "sha256:old", GooberDigest: "sha256:goober"},
			NewAxes:  tutorVersionAxes{WorkflowDigest: "sha256:new", GooberDigest: "sha256:goober"},
		}},
	}
	if err := writeTutorHoldout(root, record); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "new-"+string(rune('a'+i)), "implementation",
			"sha256:new", "sha256:goober", "model", "1.0.0", journal.PhaseCompleted)
	}
	rebuildTelemetryQueryRollup(t, root)
	db, err := openRollup(instance.NewLayout(root), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	verifyAt := time.Now().Add(8 * 24 * time.Hour)
	result, err := verifyTutorHoldouts(
		root, "example", db, 7*24*time.Hour, verifyAt.Add(-7*24*time.Hour), verifyAt,
		rollup.DefaultEfficacyThresholds(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.CanProceed || result.Findings[0].Targets[0].Verdict != rollup.EfficacyHelped {
		t.Fatalf("result = %+v, want delayed verification to retain its creation-anchored baseline", result)
	}
}

func TestVerifyTutorHoldoutDoesNotCloseWithoutImprovement(t *testing.T) {
	tests := []struct {
		name        string
		samples     int
		oldStatus   journal.RunPhase
		newStatus   journal.RunPhase
		wantState   string
		wantVerdict rollup.EfficacyVerdict
	}{
		{
			name: "insufficient data remains pending", samples: 1,
			oldStatus: journal.PhaseFailed, newStatus: journal.PhaseCompleted,
			wantState: tutorHoldoutStatePending, wantVerdict: rollup.EfficacyInsufficientData,
		},
		{
			name: "no change reopens finding", samples: 5,
			oldStatus: journal.PhaseCompleted, newStatus: journal.PhaseCompleted,
			wantState: tutorHoldoutStateReopened, wantVerdict: rollup.EfficacyNoChange,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := initDemo(t)
			record := tutorHoldoutRecord{
				Schema: tutorHoldoutSchemaVersion, ID: "sha256:record", FindingDigest: "sha256:finding",
				Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42, ChangeTypes: []tutorChangeType{tutorChangeValidation},
				State: tutorHoldoutStatePending, CreatedAt: time.Now().Add(-time.Hour),
				Targets: []tutorHoldoutTarget{{
					Workflow: "implementation",
					OldAxes:  tutorVersionAxes{WorkflowDigest: "sha256:old", GooberDigest: "sha256:goober"},
					NewAxes:  tutorVersionAxes{WorkflowDigest: "sha256:new", GooberDigest: "sha256:goober"},
				}},
			}
			record.MergedAt = &record.CreatedAt
			if err := writeTutorHoldout(root, record); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < tc.samples; i++ {
				writeEffectiveVersionFixtureRun(t, root, "before-"+string(rune('a'+i)), "implementation",
					"sha256:old", "sha256:goober", "model", "1.0.0", tc.oldStatus)
			}
			for i := 0; i < tc.samples; i++ {
				writeEffectiveVersionFixtureRun(t, root, "after-"+string(rune('a'+i)), "implementation",
					"sha256:new", "sha256:goober", "model", "1.0.0", tc.newStatus)
			}
			rebuildTelemetryQueryRollup(t, root)
			db, err := openRollup(instance.NewLayout(root), false)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()

			result, err := verifyTutorHoldouts(
				root, "example", db, 24*time.Hour, time.Now().Add(-24*time.Hour), time.Now(),
				rollup.DefaultEfficacyThresholds(),
			)
			if err != nil {
				t.Fatal(err)
			}
			if result.CanProceed || result.PendingCount != 1 {
				t.Fatalf("result = %+v, want one open finding", result)
			}
			got := result.Findings[0]
			if got.State != tc.wantState || got.Targets[0].Verdict != tc.wantVerdict {
				t.Fatalf("finding = %+v, want state=%s verdict=%s", got, tc.wantState, tc.wantVerdict)
			}
		})
	}
}

func TestTelemetryQueryTutorHoldoutsWithoutRecordsCanProceed(t *testing.T) {
	root := initDemo(t)
	t.Setenv("GOOBERS_GAGGLE", "example")
	code, stdout, stderr := runArgs(t, "telemetry-query", "--format", "tutor-live-verification", root)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, stderr)
	}
	var result tutorLiveVerificationArtifact
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("unmarshal output: %v\n%s", err, stdout)
	}
	if !result.CanProceed || !result.NoWork || result.PendingCount != 0 {
		t.Fatalf("result = %+v, want clean no-work", result)
	}
}

func TestTutorClassificationPRSectionStatesLiveVerificationPolicy(t *testing.T) {
	required := tutorClassificationPRSection(tutorChangeClassification{Types: []tutorChangeType{tutorChangeStructure}})
	if !strings.Contains(required, "**Live verification:** Required after promotion") ||
		!strings.Contains(required, "exact new EffectiveVersion cohort") {
		t.Fatalf("required section does not state mandatory exact-cohort holdout:\n%s", required)
	}
	optional := tutorClassificationPRSection(tutorChangeClassification{Types: []tutorChangeType{tutorChangePersona}})
	if !strings.Contains(optional, "**Live verification:** Optional") {
		t.Fatalf("optional section does not state persona policy:\n%s", optional)
	}
}

func writeTutorFindingFixture(t *testing.T, root, gaggle, runID string) {
	t.Helper()
	run, err := journal.Create(instance.NewLayout(root).ForGaggle(gaggle).RunsDir(), journal.RunIdentity{
		RunID: runID, Workflow: "tutor", Gaggle: gaggle,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := run.RecordStageArtifact("analyze", 1, "", "finding.md", []byte("finding")); err != nil {
		t.Fatal(err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}
