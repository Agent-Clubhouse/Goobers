package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const (
	tutorHoldoutSchemaVersion     = "goobers.dev/tutor-live-verification/v1"
	tutorHoldoutStatePending      = "pending"
	tutorHoldoutStateReopened     = "reopened"
	tutorHoldoutStateClosedHelped = "closed-helped"
)

type tutorVersionAxes struct {
	WorkflowDigest string `json:"workflowDigest"`
	GooberDigest   string `json:"gooberDigest"`
}

type tutorHoldoutTarget struct {
	Workflow         string                   `json:"workflow"`
	OldAxes          tutorVersionAxes         `json:"oldAxes"`
	NewAxes          tutorVersionAxes         `json:"newAxes"`
	OldVersion       *rollup.EffectiveVersion `json:"oldVersion,omitempty"`
	NewVersion       *rollup.EffectiveVersion `json:"newVersion,omitempty"`
	Verdict          rollup.EfficacyVerdict   `json:"verdict,omitempty"`
	FailureRateDelta float64                  `json:"failureRateDelta,omitempty"`
	Before           rollup.RunStats          `json:"before,omitempty"`
	After            rollup.RunStats          `json:"after,omitempty"`
	TransitionAt     *time.Time               `json:"transitionAt,omitempty"`
	VerificationNote string                   `json:"verificationNote,omitempty"`
}

type tutorHoldoutRecord struct {
	Schema         string               `json:"schema"`
	ID             string               `json:"id"`
	FindingDigest  string               `json:"findingDigest"`
	Gaggle         string               `json:"gaggle"`
	AuthoringRunID string               `json:"authoringRunId"`
	PRNumber       int                  `json:"prNumber,omitempty"`
	PRURL          string               `json:"prUrl,omitempty"`
	ChangeTypes    []tutorChangeType    `json:"changeTypes"`
	Targets        []tutorHoldoutTarget `json:"targets"`
	State          string               `json:"state"`
	CreatedAt      time.Time            `json:"createdAt"`
	LastCheckedAt  *time.Time           `json:"lastCheckedAt,omitempty"`
	ClosedAt       *time.Time           `json:"closedAt,omitempty"`
}

type tutorHoldoutSummary struct {
	ID            string               `json:"id"`
	FindingDigest string               `json:"findingDigest"`
	PRNumber      int                  `json:"prNumber,omitempty"`
	ChangeTypes   []tutorChangeType    `json:"changeTypes"`
	State         string               `json:"state"`
	Targets       []tutorHoldoutTarget `json:"targets"`
}

type tutorLiveVerificationArtifact struct {
	Schema       string                `json:"schema"`
	Window       string                `json:"window"`
	Since        time.Time             `json:"since"`
	Findings     []tutorHoldoutSummary `json:"findings"`
	PendingCount int                   `json:"pendingCount"`
	CanProceed   bool                  `json:"canProceed"`
	NoWork       bool                  `json:"noWork,omitempty"`
	Note         string                `json:"note,omitempty"`
}

func prepareTutorHoldout(
	root, gaggle, runID, sourceTree string,
	classification tutorChangeClassification,
	changes []tutorFileChange,
	now time.Time,
) (*tutorHoldoutRecord, error) {
	if !classification.RequiresLiveVerification() {
		return nil, nil
	}
	if strings.TrimSpace(gaggle) == "" {
		return nil, fmt.Errorf("record Tutor live verification: GOOBERS_GAGGLE is required")
	}
	if strings.TrimSpace(sourceTree) == "" {
		return nil, fmt.Errorf("record Tutor live verification: tutorConfigSource is required")
	}

	findingDigest, err := tutorFindingDigest(root, gaggle, runID)
	if err != nil {
		return nil, err
	}
	proposedSet, report, err := instance.LoadConfigDir(sourceTree)
	if err != nil {
		return nil, fmt.Errorf("load proposed Tutor config source %q: %w", sourceTree, &configReportError{report: report, err: err})
	}
	targetNames, err := tutorHoldoutTargetNames(proposedSet, gaggle, sourceTree, changes)
	if err != nil {
		return nil, err
	}
	oldVersions, err := tutorConfigVersions(instance.NewLayout(root).ConfigDir(), gaggle, targetNames)
	if err != nil {
		return nil, fmt.Errorf("resolve live pre-promotion versions: %w", err)
	}
	newVersions, err := tutorConfigVersions(sourceTree, gaggle, targetNames)
	if err != nil {
		return nil, fmt.Errorf("resolve proposed post-promotion versions: %w", err)
	}

	targets := make([]tutorHoldoutTarget, 0, len(targetNames))
	for _, name := range targetNames {
		oldAxes, oldOK := oldVersions[name]
		newAxes, newOK := newVersions[name]
		if !oldOK || !newOK {
			return nil, fmt.Errorf("mandatory live verification requires workflow %q to exist before and after the change", name)
		}
		if oldAxes == newAxes {
			continue
		}
		targets = append(targets, tutorHoldoutTarget{Workflow: name, OldAxes: oldAxes, NewAxes: newAxes})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("mandatory live verification has no observable WorkflowDigest/GooberDigest transition")
	}

	idPayload, err := json.Marshal(struct {
		FindingDigest string               `json:"findingDigest"`
		Targets       []tutorHoldoutTarget `json:"targets"`
	}{FindingDigest: findingDigest, Targets: targets})
	if err != nil {
		return nil, fmt.Errorf("encode Tutor holdout identity: %w", err)
	}
	return &tutorHoldoutRecord{
		Schema:         tutorHoldoutSchemaVersion,
		ID:             journal.Digest(idPayload),
		FindingDigest:  findingDigest,
		Gaggle:         gaggle,
		AuthoringRunID: runID,
		ChangeTypes:    append([]tutorChangeType(nil), classification.Types...),
		Targets:        targets,
		State:          tutorHoldoutStatePending,
		CreatedAt:      now.UTC(),
	}, nil
}

func tutorFindingDigest(root, gaggle, runID string) (string, error) {
	reader, err := journal.OpenRead(filepath.Join(instance.NewLayout(root).ForGaggle(gaggle).RunsDir(), runID))
	if err != nil {
		return "", fmt.Errorf("open Tutor authoring run journal: %w", err)
	}
	events, err := reader.Events()
	if err != nil {
		return "", fmt.Errorf("read Tutor authoring run journal: %w", err)
	}
	var candidates []journal.Ref
	for _, event := range events {
		if event.Type != journal.EventArtifactRecorded || event.Stage != "analyze" || event.Ref == nil {
			continue
		}
		name := strings.ToLower(event.Name)
		if strings.Contains(name, "context-manifest") || strings.HasSuffix(name, ".log") {
			continue
		}
		if strings.Contains(name, "finding") {
			return event.Ref.Digest, nil
		}
		candidates = append(candidates, *event.Ref)
	}
	if len(candidates) == 1 {
		return candidates[0].Digest, nil
	}
	return "", fmt.Errorf("record Tutor live verification: analyze stage has no unambiguous finding artifact")
}

func tutorHoldoutTargetNames(
	set *instance.ConfigSet,
	gaggle, sourceTree string,
	changes []tutorFileChange,
) ([]string, error) {
	targets := map[string]bool{}
	unknownRequiredTarget := false
	prefix := strings.Trim(path.Clean(filepath.ToSlash(sourceTree)), "/")

	addWorkflowDocument := func(raw []byte) error {
		if raw == nil {
			return nil
		}
		workflow, err := parseTutorWorkflow(raw)
		if err != nil {
			return err
		}
		if workflow != nil && workflow.Spec.Gaggle == gaggle && workflow.Name != "" {
			targets[workflow.Name] = true
		}
		return nil
	}

	for _, change := range changes {
		if isWorkflowPath(change.Path) || isWorkflowPath(change.PreviousPath) {
			if err := addWorkflowDocument(change.Before); err != nil {
				return nil, fmt.Errorf("parse changed workflow before live-verification targeting: %w", err)
			}
			if err := addWorkflowDocument(change.After); err != nil {
				return nil, fmt.Errorf("parse changed workflow after live-verification targeting: %w", err)
			}
			if change.Before == nil && change.After == nil {
				unknownRequiredTarget = true
			}
			continue
		}
		for _, changedPath := range []string{change.Path, change.PreviousPath} {
			if changedPath == "" {
				continue
			}
			relative := strings.Trim(path.Clean(filepath.ToSlash(changedPath)), "/")
			if prefix != "." && prefix != "" {
				relative = strings.TrimPrefix(relative, prefix+"/")
			}
			parts := strings.Split(relative, "/")
			if name, ok := tutorChangedGoober(parts, gaggle); ok {
				addWorkflowsUsingGoober(targets, set, gaggle, name)
				continue
			}
			if skill, ok := tutorChangedSkill(parts); ok {
				addWorkflowsUsingSkill(targets, set, gaggle, skill)
				continue
			}
			unknownRequiredTarget = true
		}
	}
	if unknownRequiredTarget {
		for _, workflow := range set.Workflows {
			if workflow.Spec.Gaggle == gaggle {
				targets[workflow.Name] = true
			}
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("mandatory live verification could not resolve an affected workflow in gaggle %q", gaggle)
	}
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func tutorChangedGoober(parts []string, gaggle string) (string, bool) {
	for i := 0; i+3 < len(parts); i++ {
		if parts[i] == "gaggles" && parts[i+1] == gaggle && parts[i+2] == "goobers" {
			return parts[i+3], true
		}
	}
	return "", false
}

func tutorChangedSkill(parts []string) (string, bool) {
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "skills" {
			return parts[i+1], true
		}
	}
	return "", false
}

func addWorkflowsUsingGoober(targets map[string]bool, set *instance.ConfigSet, gaggle, goober string) {
	for _, workflow := range set.Workflows {
		if workflow.Spec.Gaggle != gaggle {
			continue
		}
		if workflowUsesGoober(workflow, goober) {
			targets[workflow.Name] = true
		}
	}
}

func addWorkflowsUsingSkill(targets map[string]bool, set *instance.ConfigSet, gaggle, skill string) {
	goobers := map[string]bool{}
	for _, goober := range set.Goobers {
		if goober.Spec.Gaggle != gaggle {
			continue
		}
		for _, declared := range goober.Spec.Skills {
			if declared == skill {
				goobers[goober.Name] = true
				break
			}
		}
	}
	for name := range goobers {
		addWorkflowsUsingGoober(targets, set, gaggle, name)
	}
}

func workflowUsesGoober(workflow apiv1.Workflow, goober string) bool {
	for _, task := range workflow.Spec.Tasks {
		if task.Goober == goober {
			return true
		}
	}
	for _, gate := range workflow.Spec.Gates {
		if gate.Agentic != nil && gate.Agentic.Goober == goober {
			return true
		}
	}
	return false
}

func tutorConfigVersions(configDir, gaggle string, names []string) (map[string]tutorVersionAxes, error) {
	set, report, err := instance.LoadConfigDir(configDir)
	if err != nil {
		return nil, &configReportError{report: report, err: err}
	}
	goobers := goobersByName(set)
	instructions, err := loadGooberInstructions(configDir, goobers)
	if err != nil {
		return nil, err
	}
	machines, gooberDigests, err := compiledMachinesWithGooberDigests(set, goobers, instructions)
	if err != nil {
		return nil, err
	}
	out := make(map[string]tutorVersionAxes, len(names))
	for _, name := range names {
		identity := localscheduler.WorkflowIdentity{Gaggle: gaggle, Workflow: name}
		machine, ok := machines[identity]
		if !ok {
			continue
		}
		out[name] = tutorVersionAxes{
			WorkflowDigest: machine.Digest(),
			GooberDigest:   gooberDigests[identity],
		}
	}
	return out, nil
}

func writeTutorHoldout(root string, record tutorHoldoutRecord) error {
	if err := os.MkdirAll(instance.NewLayout(root).TutorHoldoutsDir(), 0o755); err != nil {
		return fmt.Errorf("create Tutor holdout directory: %w", err)
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Tutor holdout: %w", err)
	}
	data = append(data, '\n')
	if strings.TrimSpace(record.AuthoringRunID) == "" {
		return fmt.Errorf("persist Tutor holdout: authoring run id is required")
	}
	if err := journal.WriteFileAtomic(instance.NewLayout(root).TutorHoldoutPath(record.Gaggle, record.AuthoringRunID), data, 0o644); err != nil {
		return fmt.Errorf("persist Tutor holdout: %w", err)
	}
	return nil
}

func clearTutorHoldoutsForRun(root, gaggle, runID string) error {
	recordPath := instance.NewLayout(root).TutorHoldoutPath(gaggle, runID)
	if err := os.Remove(recordPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove superseded Tutor holdout for run %s: %w", runID, err)
	}
	return nil
}

func loadTutorHoldouts(root, gaggle string) ([]tutorHoldoutRecord, error) {
	dir := instance.NewLayout(root).TutorHoldoutsDir()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Tutor holdout directory: %w", err)
	}
	var records []tutorHoldoutRecord
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read Tutor holdout %s: %w", entry.Name(), err)
		}
		var record tutorHoldoutRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("parse Tutor holdout %s: %w", entry.Name(), err)
		}
		if record.Schema != tutorHoldoutSchemaVersion {
			return nil, fmt.Errorf("Tutor holdout %s has unsupported schema %q", entry.Name(), record.Schema)
		}
		if gaggle == "" || record.Gaggle == gaggle {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].ID < records[j].ID
		}
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})
	return records, nil
}

func verifyTutorHoldouts(
	root, gaggle string,
	db *rollup.DB,
	window time.Duration,
	since, now time.Time,
	thresholds rollup.EfficacyThresholds,
) (tutorLiveVerificationArtifact, error) {
	artifact := tutorLiveVerificationArtifact{
		Schema:     tutorHoldoutSchemaVersion,
		Window:     window.String(),
		Since:      since,
		CanProceed: true,
	}
	records, err := loadTutorHoldouts(root, gaggle)
	if err != nil {
		return artifact, err
	}
	if len(records) == 0 {
		artifact.NoWork = true
		artifact.Note = "no Tutor findings await live verification"
		return artifact, nil
	}

	for i := range records {
		record := &records[i]
		if record.State != tutorHoldoutStateClosedHelped {
			if record.PRNumber == 0 {
				record.State = tutorHoldoutStatePending
				for j := range record.Targets {
					record.Targets[j].Verdict = rollup.EfficacyInsufficientData
					record.Targets[j].VerificationNote = "authoring pull request has not been finalized"
				}
			} else if db == nil {
				record.State = tutorHoldoutStatePending
				for j := range record.Targets {
					record.Targets[j].Verdict = rollup.EfficacyInsufficientData
					record.Targets[j].VerificationNote = telemetryQueryNoRollupNote
				}
			} else if err := assessTutorHoldout(db, record, window, thresholds); err != nil {
				return artifact, err
			}
			checkedAt := now.UTC()
			record.LastCheckedAt = &checkedAt
			if record.State == tutorHoldoutStateClosedHelped && record.ClosedAt == nil {
				record.ClosedAt = &checkedAt
			}
			if err := writeTutorHoldout(root, *record); err != nil {
				return artifact, err
			}
		}
		if record.State != tutorHoldoutStateClosedHelped {
			artifact.PendingCount++
			artifact.CanProceed = false
		}
		artifact.Findings = append(artifact.Findings, tutorHoldoutSummary{
			ID:            record.ID,
			FindingDigest: record.FindingDigest,
			PRNumber:      record.PRNumber,
			ChangeTypes:   append([]tutorChangeType(nil), record.ChangeTypes...),
			State:         record.State,
			Targets:       append([]tutorHoldoutTarget(nil), record.Targets...),
		})
	}
	return artifact, nil
}

func assessTutorHoldout(
	db *rollup.DB,
	record *tutorHoldoutRecord,
	window time.Duration,
	thresholds rollup.EfficacyThresholds,
) error {
	anyPending := false
	anyReopened := false
	for i := range record.Targets {
		target := &record.Targets[i]
		history, err := db.DigestHistoryByEffectiveVersionForGaggle(record.Gaggle, target.Workflow)
		if err != nil {
			return fmt.Errorf("verify Tutor finding %s workflow %q: %w", record.ID, target.Workflow, err)
		}
		var matched *rollup.EffectiveVersionChange
		matchedIndex := -1
		for j := range history {
			change := &history[j]
			if change.ChangedAt.Before(record.CreatedAt) {
				continue
			}
			if effectiveVersionAxes(change.FromVersion) == target.OldAxes &&
				effectiveVersionAxes(change.ToVersion) == target.NewAxes {
				matched = change
				matchedIndex = j
				break
			}
		}
		if matched == nil {
			target.Verdict = rollup.EfficacyInsufficientData
			target.VerificationNote = "expected post-promotion EffectiveVersion cohort has not been observed"
			anyPending = true
			continue
		}

		beforeSince := record.CreatedAt.Add(-window)
		if matchedIndex > 0 {
			enteredOldCohortAt := history[matchedIndex-1].ChangedAt
			if enteredOldCohortAt.After(beforeSince) {
				beforeSince = enteredOldCohortAt
			}
		}
		afterSince := matched.ChangedAt
		var afterUntil time.Time
		if matchedIndex+1 < len(history) {
			afterUntil = history[matchedIndex+1].ChangedAt
		}
		result, err := db.AssessEfficacyByEffectiveVersion(rollup.EffectiveVersionEfficacyRequest{
			Gaggle:      record.Gaggle,
			Workflow:    target.Workflow,
			OldVersion:  matched.FromVersion,
			NewVersion:  matched.ToVersion,
			BeforeSince: beforeSince,
			AfterSince:  afterSince,
			BeforeUntil: matched.ChangedAt,
			AfterUntil:  afterUntil,
			Thresholds:  thresholds,
		})
		if err != nil {
			return fmt.Errorf("verify Tutor finding %s workflow %q efficacy: %w", record.ID, target.Workflow, err)
		}
		oldVersion, newVersion := matched.FromVersion, matched.ToVersion
		transitionAt := matched.ChangedAt
		target.OldVersion = &oldVersion
		target.NewVersion = &newVersion
		target.TransitionAt = &transitionAt
		target.Verdict = result.Verdict
		target.FailureRateDelta = result.FailureRateDelta
		target.Before = result.Before
		target.After = result.After
		target.VerificationNote = ""
		switch result.Verdict {
		case rollup.EfficacyHelped:
		case rollup.EfficacyInsufficientData:
			anyPending = true
		default:
			anyReopened = true
		}
	}

	switch {
	case anyReopened:
		record.State = tutorHoldoutStateReopened
		record.ClosedAt = nil
	case anyPending:
		record.State = tutorHoldoutStatePending
		record.ClosedAt = nil
	default:
		record.State = tutorHoldoutStateClosedHelped
	}
	return nil
}

func effectiveVersionAxes(version rollup.EffectiveVersion) tutorVersionAxes {
	return tutorVersionAxes{
		WorkflowDigest: version.WorkflowDigest,
		GooberDigest:   version.GooberDigest,
	}
}
