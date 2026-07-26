package main

import (
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
)

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
		State: tutorHoldoutStatePending, CreatedAt: createdAt,
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
		State: tutorHoldoutStatePending, CreatedAt: createdAt,
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
		State: tutorHoldoutStatePending, CreatedAt: createdAt,
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
