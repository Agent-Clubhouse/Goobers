package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

type tutorChangeType string

const (
	tutorChangePersona    tutorChangeType = "persona"
	tutorChangeGateTune   tutorChangeType = "gate-tune"
	tutorChangeStructure  tutorChangeType = "structure"
	tutorChangeSkill      tutorChangeType = "skill"
	tutorChangeValidation tutorChangeType = "validation"
)

var tutorChangeTypeOrder = map[tutorChangeType]int{
	tutorChangePersona:    0,
	tutorChangeGateTune:   1,
	tutorChangeStructure:  2,
	tutorChangeSkill:      3,
	tutorChangeValidation: 4,
}

type tutorFileChange struct {
	Path         string
	PreviousPath string
	Before       []byte
	After        []byte
}

type tutorChangeClassification struct {
	Types []tutorChangeType
}

func (c tutorChangeClassification) String() string {
	values := make([]string, len(c.Types))
	for i, changeType := range c.Types {
		values[i] = string(changeType)
	}
	return strings.Join(values, ", ")
}

func (c tutorChangeClassification) RequiresHumanSignoff() bool {
	return c.RequiresLiveVerification()
}

// RequiresLiveVerification is the TUT-A7 policy boundary: changes to
// structure, skills, or validation remain open after promotion until their
// new EffectiveVersion cohort demonstrates an improvement. Persona and gate
// tuning changes may use the normal path without a mandatory holdout.
func (c tutorChangeClassification) RequiresLiveVerification() bool {
	for _, changeType := range c.Types {
		switch changeType {
		case tutorChangeStructure, tutorChangeSkill, tutorChangeValidation:
			return true
		}
	}
	return false
}

func isTutorWorkflow(name string) bool {
	return name == "tutor" || strings.HasPrefix(name, "tutor-") || strings.HasSuffix(name, "-tutor")
}

func isTutorBranch(head, namespace string) bool {
	relative := strings.TrimPrefix(head, providers.NormalizeBranchNamespace(namespace))
	if relative == head {
		return false
	}
	workflow, _, _ := strings.Cut(relative, "/")
	return isTutorWorkflow(workflow)
}

func classifyTutorChanges(changes []tutorFileChange) (tutorChangeClassification, error) {
	if len(changes) == 0 {
		return tutorChangeClassification{}, fmt.Errorf("tutor PR has no changed files")
	}
	types := map[tutorChangeType]bool{}
	for _, change := range changes {
		changeTypes, err := classifyTutorFileChange(change)
		if err != nil {
			return tutorChangeClassification{}, fmt.Errorf("classify %s: %w", change.Path, err)
		}
		for _, changeType := range changeTypes {
			types[changeType] = true
		}
	}
	out := tutorChangeClassification{Types: make([]tutorChangeType, 0, len(types))}
	for changeType := range types {
		out.Types = append(out.Types, changeType)
	}
	sort.Slice(out.Types, func(i, j int) bool {
		return tutorChangeTypeOrder[out.Types[i]] < tutorChangeTypeOrder[out.Types[j]]
	})
	return out, nil
}

func classifyTutorFileChange(change tutorFileChange) ([]tutorChangeType, error) {
	types := map[tutorChangeType]bool{}
	paths := []string{change.Path}
	if change.PreviousPath != "" && change.PreviousPath != change.Path {
		paths = append(paths, change.PreviousPath)
		types[tutorChangeStructure] = true
	}
	workflowPaths := 0
	for _, filePath := range paths {
		switch {
		case hasPathSegment(filePath, "skills"):
			types[tutorChangeSkill] = true
		case path.Base(filePath) == "instructions.md":
			types[tutorChangePersona] = true
		case isWorkflowPath(filePath):
			workflowPaths++
		default:
			types[tutorChangeStructure] = true
		}
	}

	if workflowPaths > 0 {
		if workflowPaths != len(paths) || change.PreviousPath != "" {
			types[tutorChangeStructure] = true
		} else {
			workflowTypes, err := classifyWorkflowChange(change.Before, change.After)
			if err != nil {
				return nil, err
			}
			for _, changeType := range workflowTypes {
				types[changeType] = true
			}
		}
	}

	out := make([]tutorChangeType, 0, len(types))
	for changeType := range types {
		out = append(out, changeType)
	}
	return out, nil
}

func classifyWorkflowChange(beforeRaw, afterRaw []byte) ([]tutorChangeType, error) {
	before, err := parseTutorWorkflow(beforeRaw)
	if err != nil {
		return nil, fmt.Errorf("parse base workflow: %w", err)
	}
	after, err := parseTutorWorkflow(afterRaw)
	if err != nil {
		return nil, fmt.Errorf("parse proposed workflow: %w", err)
	}

	types := map[tutorChangeType]bool{}
	if hasAddedValidationTask(before, after) {
		types[tutorChangeValidation] = true
	}
	if before == nil || after == nil {
		types[tutorChangeStructure] = true
		return sortedTutorChangeTypes(types), nil
	}

	beforeWithoutGates := *before
	afterWithoutGates := *after
	beforeWithoutGates.Spec.Gates = nil
	afterWithoutGates.Spec.Gates = nil
	if !reflect.DeepEqual(beforeWithoutGates, afterWithoutGates) {
		beforeWithoutGoals := beforeWithoutGates
		afterWithoutGoals := afterWithoutGates
		beforeWithoutGoals.Spec.Tasks = append([]apiv1.Task(nil), beforeWithoutGates.Spec.Tasks...)
		afterWithoutGoals.Spec.Tasks = append([]apiv1.Task(nil), afterWithoutGates.Spec.Tasks...)
		for i := range beforeWithoutGoals.Spec.Tasks {
			beforeWithoutGoals.Spec.Tasks[i].Goal = ""
		}
		for i := range afterWithoutGoals.Spec.Tasks {
			afterWithoutGoals.Spec.Tasks[i].Goal = ""
		}
		if reflect.DeepEqual(beforeWithoutGoals, afterWithoutGoals) {
			types[tutorChangePersona] = true
		} else {
			types[tutorChangeStructure] = true
		}
	}

	beforeGates, ok := tutorGatesByName(before.Spec.Gates)
	if !ok {
		return nil, fmt.Errorf("base workflow contains duplicate gate names")
	}
	afterGates, ok := tutorGatesByName(after.Spec.Gates)
	if !ok {
		return nil, fmt.Errorf("proposed workflow contains duplicate gate names")
	}
	if len(beforeGates) != len(afterGates) {
		types[tutorChangeStructure] = true
	} else {
		for name, beforeGate := range beforeGates {
			afterGate, found := afterGates[name]
			if !found {
				types[tutorChangeStructure] = true
				continue
			}
			beforeAutomated := beforeGate.Automated
			afterAutomated := afterGate.Automated
			if beforeGate.Evaluator != apiv1.EvaluatorAutomated ||
				afterGate.Evaluator != apiv1.EvaluatorAutomated ||
				beforeAutomated == nil || afterAutomated == nil {
				if !reflect.DeepEqual(beforeGate, afterGate) {
					types[tutorChangeStructure] = true
				}
				continue
			}
			beforeGate.Automated = nil
			afterGate.Automated = nil
			if !reflect.DeepEqual(beforeGate, afterGate) {
				types[tutorChangeStructure] = true
				continue
			}
			if !reflect.DeepEqual(beforeAutomated, afterAutomated) {
				types[tutorChangeGateTune] = true
			}
		}
	}
	if len(types) == 0 {
		types[tutorChangeStructure] = true
	}
	return sortedTutorChangeTypes(types), nil
}

func parseTutorWorkflow(raw []byte) (*apiv1.Workflow, error) {
	if raw == nil {
		return nil, nil
	}
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(raw, &workflow); err != nil {
		return nil, err
	}
	return &workflow, nil
}

func hasAddedValidationTask(before, after *apiv1.Workflow) bool {
	if after == nil {
		return false
	}
	existing := map[string]bool{}
	if before != nil {
		for _, task := range before.Spec.Tasks {
			existing[task.Name] = true
		}
	}
	for _, task := range after.Spec.Tasks {
		if !existing[task.Name] && isValidationTask(task) {
			return true
		}
	}
	return false
}

func isValidationTask(task apiv1.Task) bool {
	var values []string
	values = append(values, task.Name, task.Goal)
	if task.Run != nil {
		values = append(values, task.Run.Command...)
	}
	text := strings.ToLower(strings.Join(values, " "))
	for _, marker := range []string{"validat", " test", "test ", "lint", "check", "make ci"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func tutorGatesByName(gates []apiv1.Gate) (map[string]apiv1.Gate, bool) {
	out := make(map[string]apiv1.Gate, len(gates))
	for _, gate := range gates {
		if _, exists := out[gate.Name]; exists {
			return nil, false
		}
		out[gate.Name] = gate
	}
	return out, true
}

func sortedTutorChangeTypes(types map[tutorChangeType]bool) []tutorChangeType {
	out := make([]tutorChangeType, 0, len(types))
	for changeType := range types {
		out = append(out, changeType)
	}
	sort.Slice(out, func(i, j int) bool {
		return tutorChangeTypeOrder[out[i]] < tutorChangeTypeOrder[out[j]]
	})
	return out
}

func hasPathSegment(filePath, segment string) bool {
	for _, part := range strings.Split(path.Clean(strings.ReplaceAll(filePath, "\\", "/")), "/") {
		if part == segment {
			return true
		}
	}
	return false
}

func isWorkflowPath(filePath string) bool {
	ext := strings.ToLower(path.Ext(filePath))
	return hasPathSegment(filePath, "workflows") && (ext == ".yaml" || ext == ".yml")
}

func localTutorChanges(base string) ([]tutorFileChange, error) {
	diffRaw, err := tutorGitOutput("diff", "--find-renames", "--find-copies-harder", "--name-status", "-z", base+"...HEAD")
	if err != nil {
		return nil, err
	}
	changes, err := parseTutorNameStatus(diffRaw)
	if err != nil {
		return nil, err
	}
	mergeBaseRaw, err := tutorGitOutput("merge-base", base, "HEAD")
	if err != nil {
		return nil, err
	}
	mergeBase := strings.TrimSpace(string(mergeBaseRaw))
	for i := range changes {
		beforePath := changes[i].Path
		if changes[i].PreviousPath != "" {
			beforePath = changes[i].PreviousPath
		}
		if isWorkflowPath(beforePath) {
			changes[i].Before, _, err = tutorGitBlob(mergeBase, beforePath)
			if err != nil {
				return nil, err
			}
		}
		if isWorkflowPath(changes[i].Path) {
			changes[i].After, _, err = tutorGitBlob("HEAD", changes[i].Path)
			if err != nil {
				return nil, err
			}
		}
	}
	return changes, nil
}

func parseTutorNameStatus(raw []byte) ([]tutorFileChange, error) {
	fields := strings.Split(string(raw), "\x00")
	var changes []tutorFileChange
	for i := 0; i < len(fields) && fields[i] != ""; {
		status := fields[i]
		i++
		if i >= len(fields) || fields[i] == "" {
			return nil, fmt.Errorf("malformed git name-status output after %q", status)
		}
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			if i+1 >= len(fields) || fields[i+1] == "" {
				return nil, fmt.Errorf("malformed git rename/copy output after %q", status)
			}
			changes = append(changes, tutorFileChange{PreviousPath: fields[i], Path: fields[i+1]})
			i += 2
			continue
		}
		changes = append(changes, tutorFileChange{Path: fields[i]})
		i++
	}
	return changes, nil
}

func tutorGitBlob(ref, filePath string) ([]byte, bool, error) {
	listed, err := tutorGitOutput("ls-tree", "-z", ref, "--", filePath)
	if err != nil {
		return nil, false, err
	}
	if len(listed) == 0 {
		return nil, false, nil
	}
	content, err := tutorGitOutput("show", ref+":"+filePath)
	if err != nil {
		return nil, false, err
	}
	return content, true, nil
}

func tutorGitOutput(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return nil, fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(exitErr.Stderr)), err)
	}
	return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
}

func classifyRemoteTutorChanges(
	ctx context.Context,
	provider remediationProvider,
	repo providers.RepositoryRef,
	pullID, baseSHA, headSHA string,
) (tutorChangeClassification, error) {
	if pullID == "" || baseSHA == "" || headSHA == "" {
		return tutorChangeClassification{}, fmt.Errorf("pull id and base/head SHAs are required")
	}
	comparison, err := provider.CompareCommits(ctx, repo, baseSHA, headSHA)
	if err != nil {
		return tutorChangeClassification{}, err
	}
	if comparison.MergeBaseSHA == "" {
		return tutorChangeClassification{}, fmt.Errorf("provider comparison returned no merge-base SHA")
	}
	files, err := provider.PullRequestFiles(ctx, repo, pullID)
	if err != nil {
		return tutorChangeClassification{}, fmt.Errorf("list pull request files: %w", err)
	}
	const githubPullRequestFileLimit = 3000
	if len(files) >= githubPullRequestFileLimit {
		return tutorChangeClassification{}, fmt.Errorf(
			"pull request file inventory reached GitHub's %d-file limit and may be incomplete",
			githubPullRequestFileLimit,
		)
	}
	changes := make([]tutorFileChange, 0, len(files))
	for _, file := range files {
		change := tutorFileChange{Path: file.Path, PreviousPath: file.PreviousPath}
		previousPath := file.Path
		if file.PreviousPath != "" {
			previousPath = file.PreviousPath
		}
		needsWorkflowContent := isWorkflowPath(file.Path) &&
			(file.PreviousPath == "" || isWorkflowPath(file.PreviousPath))
		if needsWorkflowContent && !strings.EqualFold(file.Status, "added") {
			change.Before, err = provider.RepositoryFileContent(ctx, repo, previousPath, comparison.MergeBaseSHA)
			if err != nil {
				return tutorChangeClassification{}, fmt.Errorf("read %s at merge base %s: %w", previousPath, comparison.MergeBaseSHA, err)
			}
		}
		if needsWorkflowContent && !strings.EqualFold(file.Status, "removed") {
			change.After, err = provider.RepositoryFileContent(ctx, repo, file.Path, headSHA)
			if err != nil {
				return tutorChangeClassification{}, fmt.Errorf("read %s at head %s: %w", file.Path, headSHA, err)
			}
		}
		changes = append(changes, change)
	}
	return classifyTutorChanges(changes)
}

func tutorClassificationPRSection(classification tutorChangeClassification) string {
	reviewPath := "Normal review path; this persona/gate-tune change may be auto-merged when all ordinary gates pass."
	liveVerification := "Optional for this persona/gate-tune change."
	if classification.RequiresHumanSignoff() {
		reviewPath = "Explicit human sign-off required; this structure/skill/validation change is never eligible for Goobers auto-merge."
		liveVerification = "Required after promotion; the finding remains open until telemetry from the exact new EffectiveVersion cohort reports `helped`."
	}
	return "## Tutor change classification\n\n" +
		"**Types:** `" + strings.ReplaceAll(classification.String(), ", ", "`, `") + "`\n\n" +
		"**Review path:** " + reviewPath + "\n\n" +
		"**Live verification:** " + liveVerification
}

func tutorManualReviewReason(classification tutorChangeClassification) string {
	return "Tutor change types " + strconv.Quote(classification.String()) +
		" require explicit human sign-off and are never auto-merged"
}
