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
	"github.com/goobers/goobers/internal/executor"
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
	// The starter scaffold now ships a gaggle-scoped implement/run-tests
	// package (SKILL002 fix), and scoped always wins over the instance-level
	// shared fallback (skillPackagePaths) — so the before/after transition
	// this test pins must be authored at the scoped path, or it is masked by
	// the (identical, copied) scoped package on both sides.
	liveSkillPath := filepath.Join(liveConfig, "gaggles", gaggle, "skills", "implement", "SKILL.md")
	before := []byte("# Implement\n\nUse the original approach.\n")
	if err := os.WriteFile(liveSkillPath, before, 0o644); err != nil {
		t.Fatal(err)
	}

	proposedRoot := t.TempDir()
	proposedConfig := filepath.Join(proposedRoot, "proposed")
	if err := os.CopyFS(proposedConfig, os.DirFS(liveConfig)); err != nil {
		t.Fatalf("copy config: %v", err)
	}
	proposedSkillPath := filepath.Join(proposedConfig, "gaggles", gaggle, "skills", "implement", "SKILL.md")
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

func TestRefreshTutorHoldoutMergeStateDiscardsClosedUnmergedPR(t *testing.T) {
	root := initDemo(t)
	record := tutorHoldoutRecord{
		Schema: tutorHoldoutSchemaVersion, ID: "sha256:abandoned", FindingDigest: "sha256:finding",
		Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42,
		State: tutorHoldoutStatePending, CreatedAt: time.Now().UTC(),
	}
	if err := writeTutorHoldout(root, record); err != nil {
		t.Fatal(err)
	}
	if err := refreshTutorHoldoutMergeState(
		context.Background(),
		root,
		"example",
		providers.RepositoryRef{Owner: "acme", Name: "app"},
		tutorHoldoutPollerStub{result: providers.PullRequestPollResult{
			Number: 42, State: "closed",
		}},
	); err != nil {
		t.Fatal(err)
	}
	records, err := loadTutorHoldouts(root, "example")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %+v, want abandoned holdout removed", records)
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

func TestOpenPRDiscardsPreparedTutorHoldoutWhenProviderFails(t *testing.T) {
	root := initDemo(t)
	const (
		gaggle = "example"
		runID  = "failed-open"
	)
	repoDir, proposedConfig := gitTutorConfigWorktree(
		t,
		instance.NewLayout(root).ConfigDir(),
		func(configDir string) {
			sourcePath := filepath.Join(configDir, "gaggles", gaggle, "workflows", "default-implement.yaml")
			raw, err := os.ReadFile(sourcePath)
			if err != nil {
				t.Fatal(err)
			}
			updated := strings.Replace(string(raw), "name: default-implement", "name: added-implement", 1)
			if updated == string(raw) {
				t.Fatal("workflow fixture metadata name was not replaced")
			}
			addedPath := filepath.Join(configDir, "gaggles", gaggle, "workflows", "added-implement.yaml")
			if err := os.WriteFile(addedPath, []byte(updated), 0o644); err != nil {
				t.Fatal(err)
			}
			gooberPath := filepath.Join(configDir, "gaggles", gaggle, "goobers", "coder", "goober.yaml")
			gooberRaw, err := os.ReadFile(gooberPath)
			if err != nil {
				t.Fatal(err)
			}
			updatedGoober := strings.Replace(
				string(gooberRaw),
				"    - default-implement",
				"    - default-implement\n    - added-implement",
				1,
			)
			if updatedGoober == string(gooberRaw) {
				t.Fatal("goober fixture workflow list was not updated")
			}
			if err := os.WriteFile(gooberPath, []byte(updatedGoober), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	)
	writeTutorFindingFixture(t, root, gaggle, runID)
	server := newFakeGitHubServer(t, "your-org", "your-repo")
	providerCmdEnv(t, server, "GOOBERS_CRED_PROVIDER_PR_WRITE", runID)
	// The server is closed below so every call fails at the transport; the
	// assertion is on the discarded holdout, not on retry timing. Spend the
	// transient-retry budget so open-pr fails fast instead of burning
	// 1+2+4+8 = 15s of real backoff sleep. providerCmdEnv's own t.Cleanup
	// still restores the original factory.
	baseFactory := newGitHubProvider
	newGitHubProvider = func(token string, opts ...func(*providers.GitHubProvider)) *providers.GitHubProvider {
		return baseFactory(token, append(opts, providers.WithMaxTransientRetries(0))...)
	}
	t.Setenv("GOOBERS_WORKFLOW", "tutor")
	t.Setenv("GOOBERS_GAGGLE", gaggle)
	t.Setenv(executor.InputEnvVar("recordLiveVerification"), "true")
	t.Setenv(executor.InputEnvVar("tutorConfigSource"), proposedConfig)
	t.Chdir(repoDir)
	server.server.Close()

	code, _, stderr := runArgs(t, "open-pr", root)
	if code != 1 || !strings.Contains(stderr, "open pull request") {
		t.Fatalf("open-pr code = %d, want provider failure", code)
	}
	records, err := loadTutorHoldouts(root, gaggle)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %+v, want failed-open holdout removed", records)
	}
}

func TestVerifyTutorHoldoutUsesPinnedTransitionNotLatest(t *testing.T) {
	root := initDemo(t)
	const workflowName = "default-implement"
	liveVersions, err := tutorConfigVersions(
		instance.NewLayout(root).ConfigDir(),
		"example",
		[]string{workflowName},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	liveAxes := liveVersions[workflowName]
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
			Workflow: workflowName,
			OldAxes:  tutorVersionAxes{WorkflowDigest: "sha256:old", GooberDigest: "sha256:goober-old"},
			NewAxes:  liveAxes,
		}},
	}
	if err := writeTutorHoldout(root, record); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "old-"+string(rune('a'+i)), workflowName,
			"sha256:old", "sha256:goober-old", "model-a", "1.0.0", journal.PhaseFailed)
	}
	// Same workflow name in another gaggle must not break this gaggle's
	// old->new transition or contribute efficacy samples.
	writeEffectiveVersionFixtureRunForGaggle(
		t, root, "other", "foreign", workflowName,
		"sha256:foreign", "sha256:foreign-goober", "foreign-model", "9.0.0", journal.PhaseCompleted,
	)
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "new-"+string(rune('a'+i)), workflowName,
			liveAxes.WorkflowDigest, liveAxes.GooberDigest, "model-a", "1.0.0", journal.PhaseCompleted)
	}

	// A later model/harness cohort regresses without changing the promoted
	// configuration axes. The holdout remains pinned to the promotion cohort.
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "later-"+string(rune('a'+i)), workflowName,
			liveAxes.WorkflowDigest, liveAxes.GooberDigest, "model-b", "2.0.0", journal.PhaseFailed)
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
	if target.NewVersion == nil || effectiveVersionAxes(*target.NewVersion) != liveAxes || target.Verdict != rollup.EfficacyHelped {
		t.Fatalf("target = %+v, want pinned new cohort with helped verdict", target)
	}
}

func TestVerifyTutorHoldoutUsesFinalAmendedTransitionAfterInterveningPromotion(t *testing.T) {
	root := initDemo(t)
	const workflowName = "default-implement"
	liveConfig := instance.NewLayout(root).ConfigDir()
	workflowPath := filepath.Join(liveConfig, "gaggles", "example", "workflows", workflowName+".yaml")
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
	finalVersions, err := tutorConfigVersions(liveConfig, "example", []string{workflowName}, nil, nil, nil)
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
	mergedAt := time.Now().UTC()
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(
			t, root, "intervening-"+string(rune('a'+i)), workflowName,
			interveningAxes.WorkflowDigest, interveningAxes.GooberDigest, "model", "1.0.0", journal.PhaseCompleted,
		)
	}
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(
			t, root, "final-"+string(rune('a'+i)), workflowName,
			finalAxes.WorkflowDigest, finalAxes.GooberDigest, "model", "1.0.0", journal.PhaseFailed,
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
	if result.CanProceed || result.Findings[0].State != tutorHoldoutStateReopened {
		t.Fatalf("result = %+v, want regressed final amended transition to reopen", result)
	}
	target := result.Findings[0].Targets[0]
	if target.OldAxes != interveningAxes || target.NewAxes != finalAxes {
		t.Fatalf("refreshed axes = %+v -> %+v, want %+v -> %+v", target.OldAxes, target.NewAxes, interveningAxes, finalAxes)
	}
	if target.Verdict != rollup.EfficacyRegressed {
		t.Fatalf("verdict = %q, want final cohort regression", target.Verdict)
	}
	if target.NewAxes == proposalAxes {
		t.Fatalf("new axes retained stale proposal value: %+v", target.NewAxes)
	}
}

func TestVerifyTutorHoldoutWorkflowLifecycleSemantics(t *testing.T) {
	t.Run("addition requires healthy post-promotion cohort", func(t *testing.T) {
		root := initDemo(t)
		const workflowName = "added-workflow"
		axes := addLiveTutorWorkflow(t, root, workflowName)
		mergedAt := time.Now().UTC()
		record := tutorHoldoutRecord{
			Schema: tutorHoldoutSchemaVersion, ID: "sha256:addition", FindingDigest: "sha256:finding",
			Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42, MergedAt: &mergedAt,
			State: tutorHoldoutStatePending, CreatedAt: mergedAt.Add(-time.Hour),
			Targets: []tutorHoldoutTarget{{
				Workflow: workflowName, Lifecycle: tutorHoldoutAddition, NewAxes: axes,
			}},
		}
		if err := writeTutorHoldout(root, record); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			writeEffectiveVersionFixtureRun(
				t, root, "added-"+string(rune('a'+i)), workflowName,
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

func TestVerifyTutorHoldoutAdditionUsesFinalLiveCohort(t *testing.T) {
	root := initDemo(t)
	const workflowName = "added-workflow"
	liveAxes := addLiveTutorWorkflow(t, root, workflowName)
	mergedAt := time.Now().UTC()
	record := tutorHoldoutRecord{
		Schema: tutorHoldoutSchemaVersion, ID: "sha256:addition-final", FindingDigest: "sha256:finding",
		Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42, MergedAt: &mergedAt,
		State: tutorHoldoutStatePending, CreatedAt: mergedAt.Add(-time.Hour),
		Targets: []tutorHoldoutTarget{{
			Workflow: workflowName, Lifecycle: tutorHoldoutAddition, NewAxes: liveAxes,
		}},
	}
	if err := writeTutorHoldout(root, record); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(
			t, root, "first-live-"+string(rune('a'+i)), workflowName,
			liveAxes.WorkflowDigest, liveAxes.GooberDigest, "model", "1.0.0", journal.PhaseCompleted,
		)
	}
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(
			t, root, "intervening-"+string(rune('a'+i)), workflowName,
			"sha256:intervening", liveAxes.GooberDigest, "model", "1.0.0", journal.PhaseCompleted,
		)
	}
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(
			t, root, "final-live-"+string(rune('a'+i)), workflowName,
			liveAxes.WorkflowDigest, liveAxes.GooberDigest, "model", "1.0.0", journal.PhaseFailed,
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
	target := result.Findings[0].Targets[0]
	if result.CanProceed || target.Verdict != rollup.EfficacyRegressed || target.After.FailedRuns != 5 {
		t.Fatalf("result = %+v, want final live addition cohort regression", result)
	}
}

func TestVerifyTutorHoldoutExcludesEarlierRunsFromRepromotedCohort(t *testing.T) {
	root := initDemo(t)
	const workflowName = "default-implement"
	liveAxes := testLiveTutorAxes(t, root, workflowName)
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "initial-old-"+string(rune('a'+i)), workflowName,
			"sha256:old", liveAxes.GooberDigest, "model", "1.0.0", journal.PhaseFailed)
	}

	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "earlier-new-"+string(rune('a'+i)), workflowName,
			liveAxes.WorkflowDigest, liveAxes.GooberDigest, "model", "1.0.0", journal.PhaseCompleted)
	}
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "reverted-old-"+string(rune('a'+i)), workflowName,
			"sha256:old", liveAxes.GooberDigest, "model", "1.0.0", journal.PhaseFailed)
	}

	createdAt := time.Now().UTC()
	record := tutorHoldoutRecord{
		Schema: tutorHoldoutSchemaVersion, ID: "sha256:repromoted", FindingDigest: "sha256:finding",
		Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42, ChangeTypes: []tutorChangeType{tutorChangeStructure},
		State: tutorHoldoutStatePending, CreatedAt: createdAt, MergedAt: &createdAt,
		Targets: []tutorHoldoutTarget{{
			Workflow: workflowName,
			OldAxes:  tutorVersionAxes{WorkflowDigest: "sha256:old", GooberDigest: liveAxes.GooberDigest},
			NewAxes:  liveAxes,
		}},
	}
	if err := writeTutorHoldout(root, record); err != nil {
		t.Fatal(err)
	}
	writeEffectiveVersionFixtureRun(t, root, "repromoted-new", workflowName,
		liveAxes.WorkflowDigest, liveAxes.GooberDigest, "model", "1.0.0", journal.PhaseCompleted)
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

func TestVerifyTutorHoldoutUsesFinalLiveCohortReentry(t *testing.T) {
	root := initDemo(t)
	const workflowName = "default-implement"
	liveAxes := testLiveTutorAxes(t, root, workflowName)
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "old-"+string(rune('a'+i)), workflowName,
			"sha256:old", liveAxes.GooberDigest, "model", "1.0.0", journal.PhaseFailed)
	}
	createdAt := time.Now().UTC()
	record := tutorHoldoutRecord{
		Schema: tutorHoldoutSchemaVersion, ID: "sha256:reentry", FindingDigest: "sha256:finding",
		Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42, ChangeTypes: []tutorChangeType{tutorChangeStructure},
		State: tutorHoldoutStatePending, CreatedAt: createdAt, MergedAt: &createdAt,
		Targets: []tutorHoldoutTarget{{
			Workflow: workflowName,
			OldAxes:  tutorVersionAxes{WorkflowDigest: "sha256:old", GooberDigest: liveAxes.GooberDigest},
			NewAxes:  liveAxes,
		}},
	}
	if err := writeTutorHoldout(root, record); err != nil {
		t.Fatal(err)
	}
	writeEffectiveVersionFixtureRun(t, root, "first-new", workflowName,
		liveAxes.WorkflowDigest, liveAxes.GooberDigest, "model", "1.0.0", journal.PhaseCompleted)
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "intervening-"+string(rune('a'+i)), workflowName,
			"sha256:other", liveAxes.GooberDigest, "model", "1.0.0", journal.PhaseFailed)
	}
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "later-new-"+string(rune('a'+i)), workflowName,
			liveAxes.WorkflowDigest, liveAxes.GooberDigest, "model", "1.0.0", journal.PhaseCompleted)
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
	if target.After.TotalRuns != 5 || target.Verdict != rollup.EfficacyHelped {
		t.Fatalf("target = %+v, want final live cohort reentry", target)
	}
}

func TestVerifyTutorHoldoutBaselineWindowIsAnchoredToFinding(t *testing.T) {
	root := initDemo(t)
	const workflowName = "default-implement"
	liveAxes := testLiveTutorAxes(t, root, workflowName)
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "old-"+string(rune('a'+i)), workflowName,
			"sha256:old", liveAxes.GooberDigest, "model", "1.0.0", journal.PhaseFailed)
	}
	createdAt := time.Now().UTC()
	record := tutorHoldoutRecord{
		Schema: tutorHoldoutSchemaVersion, ID: "sha256:delayed", FindingDigest: "sha256:finding",
		Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42, ChangeTypes: []tutorChangeType{tutorChangeStructure},
		State: tutorHoldoutStatePending, CreatedAt: createdAt, MergedAt: &createdAt,
		Targets: []tutorHoldoutTarget{{
			Workflow: workflowName,
			OldAxes:  tutorVersionAxes{WorkflowDigest: "sha256:old", GooberDigest: liveAxes.GooberDigest},
			NewAxes:  liveAxes,
		}},
	}
	if err := writeTutorHoldout(root, record); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		writeEffectiveVersionFixtureRun(t, root, "new-"+string(rune('a'+i)), workflowName,
			liveAxes.WorkflowDigest, liveAxes.GooberDigest, "model", "1.0.0", journal.PhaseCompleted)
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
			const workflowName = "default-implement"
			liveAxes := testLiveTutorAxes(t, root, workflowName)
			record := tutorHoldoutRecord{
				Schema: tutorHoldoutSchemaVersion, ID: "sha256:record", FindingDigest: "sha256:finding",
				Gaggle: "example", AuthoringRunID: "authoring", PRNumber: 42, ChangeTypes: []tutorChangeType{tutorChangeValidation},
				State: tutorHoldoutStatePending, CreatedAt: time.Now().Add(-time.Hour),
				Targets: []tutorHoldoutTarget{{
					Workflow: workflowName,
					OldAxes:  tutorVersionAxes{WorkflowDigest: "sha256:old", GooberDigest: liveAxes.GooberDigest},
					NewAxes:  liveAxes,
				}},
			}
			record.MergedAt = &record.CreatedAt
			if err := writeTutorHoldout(root, record); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < tc.samples; i++ {
				writeEffectiveVersionFixtureRun(t, root, "before-"+string(rune('a'+i)), workflowName,
					"sha256:old", liveAxes.GooberDigest, "model", "1.0.0", tc.oldStatus)
			}
			for i := 0; i < tc.samples; i++ {
				writeEffectiveVersionFixtureRun(t, root, "after-"+string(rune('a'+i)), workflowName,
					liveAxes.WorkflowDigest, liveAxes.GooberDigest, "model", "1.0.0", tc.newStatus)
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

func testLiveTutorAxes(t *testing.T, root, workflow string) tutorVersionAxes {
	t.Helper()
	versions, err := tutorConfigVersions(
		instance.NewLayout(root).ConfigDir(),
		"example",
		[]string{workflow},
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	axes, ok := versions[workflow]
	if !ok {
		t.Fatalf("live workflow %q not found", workflow)
	}
	return axes
}

func addLiveTutorWorkflow(t *testing.T, root, workflow string) tutorVersionAxes {
	t.Helper()
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
	definition.Name = workflow
	raw, err = yaml.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(liveConfig, "gaggles", "example", "workflows", workflow+".yaml"),
		raw,
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	return testLiveTutorAxes(t, root, workflow)
}
