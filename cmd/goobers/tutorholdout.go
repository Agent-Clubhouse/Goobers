package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/internal/telemetry/rollup"
	"github.com/goobers/goobers/providers"
)

// tutorConfigVersionsModelCredential builds the same instance-configured
// agent:model resolver admission uses elsewhere (#4292), so Tutor's own
// version-axis model discovery sees a file/keychain/store-sourced credential
// instead of only an ambient env var.
func tutorConfigVersionsModelCredential(cfg *instance.Config) (func(ctx context.Context) (string, error), error) {
	stores, err := secretstore.NewRegistry(cfg.SecretStores)
	if err != nil {
		return nil, fmt.Errorf("load secret stores for Tutor version resolution: %w", err)
	}
	return agentModelCredentialResolver(cfg, stores)
}

const (
	tutorHoldoutSchemaVersion     = "goobers.dev/tutor-live-verification/v1"
	tutorHoldoutStatePending      = "pending"
	tutorHoldoutStateReopened     = "reopened"
	tutorHoldoutStateClosedHelped = "closed-helped"
)

type tutorHoldoutLifecycle string

const (
	tutorHoldoutTransition tutorHoldoutLifecycle = "transition"
	tutorHoldoutAddition   tutorHoldoutLifecycle = "addition"
	tutorHoldoutRemoval    tutorHoldoutLifecycle = "removal"
)

type tutorVersionAxes struct {
	WorkflowDigest string `json:"workflowDigest"`
	GooberDigest   string `json:"gooberDigest"`
}

type tutorHoldoutTarget struct {
	Workflow         string                   `json:"workflow"`
	Lifecycle        tutorHoldoutLifecycle    `json:"lifecycle"`
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
	MergedAt       *time.Time           `json:"mergedAt,omitempty"`
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
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load instance config for Tutor version resolution: %w", err)
	}
	tutorModelCredential, err := tutorConfigVersionsModelCredential(cfg)
	if err != nil {
		return nil, err
	}
	oldVersions, err := tutorConfigVersions(instance.NewLayout(root).ConfigDir(), gaggle, targetNames, cfg.Runner.EnvPassthrough, cfg.Runner.HarnessCommand, tutorModelCredential)
	if err != nil {
		return nil, fmt.Errorf("resolve live pre-promotion versions: %w", err)
	}
	newVersions, err := tutorConfigVersions(sourceTree, gaggle, targetNames, cfg.Runner.EnvPassthrough, cfg.Runner.HarnessCommand, tutorModelCredential)
	if err != nil {
		return nil, fmt.Errorf("resolve proposed post-promotion versions: %w", err)
	}

	targets := make([]tutorHoldoutTarget, 0, len(targetNames))
	for _, name := range targetNames {
		oldAxes, oldOK := oldVersions[name]
		newAxes, newOK := newVersions[name]
		if !oldOK && !newOK {
			continue
		}
		lifecycle := tutorHoldoutTransition
		switch {
		case !oldOK:
			lifecycle = tutorHoldoutAddition
		case !newOK:
			lifecycle = tutorHoldoutRemoval
		case oldAxes == newAxes:
			continue
		}
		targets = append(targets, tutorHoldoutTarget{
			Workflow: name, Lifecycle: lifecycle, OldAxes: oldAxes, NewAxes: newAxes,
		})
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("mandatory live verification has no observable workflow lifecycle or WorkflowDigest/GooberDigest transition")
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

func tutorConfigVersions(configDir, gaggle string, names, envPassthrough []string, harnessCommand map[string][]string, modelCredential func(ctx context.Context) (string, error)) (map[string]tutorVersionAxes, error) {
	set, report, err := instance.LoadConfigDir(configDir)
	if err != nil {
		return nil, &configReportError{report: report, err: err}
	}
	goobers := goobersByName(set)
	instructions, err := loadGooberInstructions(configDir, goobers)
	if err != nil {
		return nil, err
	}
	machines, gooberDigests, _, _, err := compiledMachinesWithGooberDigestsAndWarnings(
		configDir, set, goobers, instructions, envPassthrough, harnessCommand,
		false, modelCredential,
	)
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
			return nil, fmt.Errorf("tutor holdout %s has unsupported schema %q", entry.Name(), record.Schema)
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

type tutorPullRequestPoller interface {
	PollPullRequest(context.Context, providers.PullRequestPollRequest) (providers.PullRequestPollResult, error)
}

func refreshTutorHoldoutMergeStateFromProvider(root, gaggle string) error {
	records, err := loadTutorHoldouts(root, gaggle)
	if err != nil {
		return err
	}
	needsPoll := false
	for _, record := range records {
		if record.PRNumber != 0 && record.MergedAt == nil {
			needsPoll = true
			break
		}
	}
	if !needsPoll {
		return nil
	}
	repo, err := providerRepo(root)
	if err != nil {
		return err
	}
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		return err
	}
	ctx, cancel := providerCommandContext()
	defer cancel()
	return refreshTutorHoldoutMergeState(
		ctx,
		root,
		gaggle,
		repo,
		newGitHubProvider(token),
	)
}

func refreshTutorHoldoutMergeState(
	ctx context.Context,
	root, gaggle string,
	repo providers.RepositoryRef,
	poller tutorPullRequestPoller,
) error {
	records, err := loadTutorHoldouts(root, gaggle)
	if err != nil {
		return err
	}
	for i := range records {
		record := &records[i]
		if record.PRNumber == 0 || record.MergedAt != nil {
			continue
		}
		poll, err := poller.PollPullRequest(ctx, providers.PullRequestPollRequest{
			Repository: repo,
			PullID:     strconv.Itoa(record.PRNumber),
		})
		if err != nil {
			return fmt.Errorf("poll Tutor holdout pull request #%d: %w", record.PRNumber, err)
		}
		if !poll.Merged {
			if strings.EqualFold(poll.State, "closed") {
				if err := clearTutorHoldoutsForRun(root, record.Gaggle, record.AuthoringRunID); err != nil {
					return fmt.Errorf("discard abandoned Tutor holdout for pull request #%d: %w", record.PRNumber, err)
				}
			}
			continue
		}
		if poll.MergedAt == nil {
			return fmt.Errorf("poll Tutor holdout pull request #%d: merged response has no merge time", record.PRNumber)
		}
		mergedAt := poll.MergedAt.UTC()
		record.MergedAt = &mergedAt
		record.State = tutorHoldoutStatePending
		record.ClosedAt = nil
		for j := range record.Targets {
			resetTutorHoldoutAssessment(&record.Targets[j])
		}
		if err := writeTutorHoldout(root, *record); err != nil {
			return err
		}
	}
	return nil
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
			if record.PRNumber == 0 || record.MergedAt == nil {
				record.State = tutorHoldoutStatePending
				for j := range record.Targets {
					record.Targets[j].Verdict = rollup.EfficacyInsufficientData
					record.Targets[j].VerificationNote = "authoring pull request has not been finalized"
				}
			} else {
				liveVersions, err := reconcileTutorHoldoutTargets(root, record)
				if err != nil {
					return artifact, err
				}
				if err := assessTutorHoldout(db, record, liveVersions, window, now, thresholds); err != nil {
					return artifact, err
				}
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

func reconcileTutorHoldoutTargets(root string, record *tutorHoldoutRecord) (map[string]tutorVersionAxes, error) {
	names := make([]string, len(record.Targets))
	for i := range record.Targets {
		names[i] = record.Targets[i].Workflow
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		return nil, fmt.Errorf("load instance config for Tutor reconciliation: %w", err)
	}
	tutorModelCredential, err := tutorConfigVersionsModelCredential(cfg)
	if err != nil {
		return nil, err
	}
	liveVersions, err := tutorConfigVersions(
		instance.NewLayout(root).ConfigDir(), record.Gaggle, names, cfg.Runner.EnvPassthrough, cfg.Runner.HarnessCommand, tutorModelCredential,
	)
	if err != nil {
		return nil, fmt.Errorf("resolve live reconciled Tutor config: %w", err)
	}
	return liveVersions, nil
}

func assessTutorHoldout(
	db *rollup.DB,
	record *tutorHoldoutRecord,
	liveVersions map[string]tutorVersionAxes,
	window time.Duration,
	now time.Time,
	thresholds rollup.EfficacyThresholds,
) error {
	anyPending := false
	anyReopened := false
	for i := range record.Targets {
		target := &record.Targets[i]
		resetTutorHoldoutAssessment(target)
		liveAxes, liveExists := liveVersions[target.Workflow]
		lifecycle := target.Lifecycle
		switch {
		case target.OldAxes == (tutorVersionAxes{}):
			lifecycle = tutorHoldoutAddition
		case lifecycle == tutorHoldoutRemoval:
		case !liveExists:
			lifecycle = tutorHoldoutRemoval
			target.NewAxes = tutorVersionAxes{}
		default:
			lifecycle = tutorHoldoutTransition
		}
		target.Lifecycle = lifecycle
		switch lifecycle {
		case tutorHoldoutAddition:
			if !liveExists {
				target.Verdict = rollup.EfficacyInsufficientData
				target.VerificationNote = "added workflow is not present in the live reconciled configuration"
				anyPending = true
				continue
			}
			target.NewAxes = liveAxes
			if db == nil {
				target.Verdict = rollup.EfficacyInsufficientData
				target.VerificationNote = telemetryQueryNoRollupNote
				anyPending = true
				continue
			}
			history, err := db.DigestHistoryByEffectiveVersionForGaggle(context.Background(), record.Gaggle, target.Workflow)
			if err != nil {
				return fmt.Errorf("verify Tutor finding %s added workflow %q: %w", record.ID, target.Workflow, err)
			}
			cohortSince := *record.MergedAt
			if final, _ := latestTutorConfigTransitionAfter(history, *record.MergedAt); final != nil {
				if effectiveVersionAxes(final.ToVersion) != liveAxes {
					target.Verdict = rollup.EfficacyInsufficientData
					target.VerificationNote = "added workflow final observed configuration does not match the live reconciled configuration"
					anyPending = true
					continue
				}
				cohortSince = final.ChangedAt
			}
			cohort, err := db.FirstEffectiveVersionCohortForGaggle(
				context.Background(),
				record.Gaggle,
				target.Workflow,
				liveAxes.WorkflowDigest,
				liveAxes.GooberDigest,
				cohortSince,
			)
			if err != nil {
				return fmt.Errorf("verify Tutor finding %s added workflow %q: %w", record.ID, target.Workflow, err)
			}
			if cohort == nil {
				target.Verdict = rollup.EfficacyInsufficientData
				target.VerificationNote = "added workflow has no post-promotion EffectiveVersion cohort"
				anyPending = true
				continue
			}
			version := cohort.Version
			transitionAt := cohort.StartedAt
			target.NewAxes = effectiveVersionAxes(version)
			target.NewVersion = &version
			target.TransitionAt = &transitionAt
			target.After = cohort.Stats
			terminal := cohort.Stats.CompletedRuns + cohort.Stats.FailedRuns
			switch {
			case terminal < thresholds.MinSamples:
				target.Verdict = rollup.EfficacyInsufficientData
				target.VerificationNote = "added workflow post-promotion cohort has insufficient terminal runs"
				anyPending = true
			case cohort.Stats.FailedRuns > 0:
				target.Verdict = rollup.EfficacyRegressed
				target.VerificationNote = "added workflow post-promotion cohort contains failed runs"
				anyReopened = true
			default:
				target.Verdict = rollup.EfficacyHelped
			}
			continue
		case tutorHoldoutRemoval:
			if _, exists := liveVersions[target.Workflow]; exists {
				target.Verdict = rollup.EfficacyInsufficientData
				target.VerificationNote = "removed workflow is still present in the live reconciled configuration"
				anyPending = true
				continue
			}
			removedAt := now.UTC()
			target.TransitionAt = &removedAt
			target.Verdict = rollup.EfficacyHelped
			target.VerificationNote = "workflow removal is reconciled; no post-change cohort exists by definition"
			continue
		case tutorHoldoutTransition:
		default:
			return fmt.Errorf("verify Tutor finding %s workflow %q: unknown lifecycle %q", record.ID, target.Workflow, lifecycle)
		}
		if db == nil {
			target.Verdict = rollup.EfficacyInsufficientData
			target.VerificationNote = telemetryQueryNoRollupNote
			anyPending = true
			continue
		}
		history, err := db.DigestHistoryByEffectiveVersionForGaggle(context.Background(), record.Gaggle, target.Workflow)
		if err != nil {
			return fmt.Errorf("verify Tutor finding %s workflow %q: %w", record.ID, target.Workflow, err)
		}
		matched, matchedIndex := latestTutorConfigTransitionAfter(history, *record.MergedAt)
		if matched == nil || effectiveVersionAxes(matched.ToVersion) != liveAxes {
			target.Verdict = rollup.EfficacyInsufficientData
			target.VerificationNote = "live reconciled post-promotion configuration transition has not been observed"
			anyPending = true
			continue
		}
		target.OldAxes = effectiveVersionAxes(matched.FromVersion)
		target.NewAxes = effectiveVersionAxes(matched.ToVersion)

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
		result, err := db.AssessEfficacyByEffectiveVersion(context.Background(), rollup.EffectiveVersionEfficacyRequest{
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

func latestTutorConfigTransitionAfter(
	history []rollup.EffectiveVersionChange,
	mergedAt time.Time,
) (*rollup.EffectiveVersionChange, int) {
	var final *rollup.EffectiveVersionChange
	finalIndex := -1
	for i := range history {
		change := &history[i]
		if change.ChangedAt.Before(mergedAt) ||
			effectiveVersionAxes(change.FromVersion) == effectiveVersionAxes(change.ToVersion) {
			continue
		}
		final = change
		finalIndex = i
	}
	return final, finalIndex
}

func resetTutorHoldoutAssessment(target *tutorHoldoutTarget) {
	target.OldVersion = nil
	target.NewVersion = nil
	target.Verdict = ""
	target.FailureRateDelta = 0
	target.Before = rollup.RunStats{}
	target.After = rollup.RunStats{}
	target.TransitionAt = nil
	target.VerificationNote = ""
}

func effectiveVersionAxes(version rollup.EffectiveVersion) tutorVersionAxes {
	return tutorVersionAxes{
		WorkflowDigest: version.WorkflowDigest,
		GooberDigest:   version.GooberDigest,
	}
}
