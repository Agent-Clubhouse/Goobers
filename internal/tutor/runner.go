package tutor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/providers"
)

// RunnerConfig controls one Tutor analysis-and-PR cycle.
type RunnerConfig struct {
	Repository   providers.RepositoryRef
	BaseBranch   string
	BaseSHA      string
	BranchPrefix string
	Query        Query
	Reviewers    []string
	Draft        bool
}

// Runner wires telemetry analysis to provider-backed config PR creation.
type Runner struct {
	Store    TelemetryStore
	Provider providers.RepoProvider
	Analyzer Analyzer
	Planner  Planner
	Recorder Recorder
	Config   RunnerConfig
}

// RunResult describes the outcome of one Tutor cycle.
type RunResult struct {
	SignalCount int
	Findings    []Finding
	Proposal    *Proposal
	PullRequest *providers.PullRequestResult
	Opened      bool
}

// Run mines telemetry, proposes the highest-priority config improvement, and
// opens a provider pull request when there is an actionable signal.
func (r Runner) Run(ctx context.Context) (RunResult, error) {
	if r.Store == nil {
		return RunResult{}, errors.New("tutor runner requires telemetry store")
	}
	recorder := r.Recorder
	if recorder == nil {
		recorder = noopRecorder{}
	}
	signals, err := r.Store.QuerySignals(ctx, r.Config.Query)
	if err != nil {
		return RunResult{}, fmt.Errorf("query tutor telemetry: %w", err)
	}
	analyzer := r.Analyzer
	if analyzer.Thresholds == (Thresholds{}) {
		analyzer = NewAnalyzer(Thresholds{})
	}
	findings := analyzer.Analyze(signals)
	result := RunResult{SignalCount: len(signals), Findings: findings}
	if len(findings) == 0 {
		recorder.RecordNoSignal(ctx, len(signals))
		return result, nil
	}
	if r.Provider == nil {
		return RunResult{}, errors.New("tutor runner requires repo provider for actionable findings")
	}
	if r.Config.Repository.Name == "" {
		return RunResult{}, errors.New("tutor runner requires config repository")
	}

	planner := r.Planner
	if r.Config.BranchPrefix != "" {
		planner.BranchPrefix = r.Config.BranchPrefix
	}
	proposal := planner.Propose(findings[0])
	baseBranch := r.Config.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}
	if err := r.mergeWithExistingConfig(ctx, baseBranch, &proposal); err != nil {
		return RunResult{}, err
	}
	branch, err := r.Provider.CreateBranch(ctx, providers.BranchRequest{
		Repository: r.Config.Repository,
		BaseBranch: baseBranch,
		BaseSHA:    r.Config.BaseSHA,
		Name:       proposal.BranchName,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("create tutor proposal branch: %w", err)
	}
	branchName := branch.Name
	if branchName == "" {
		branchName = proposal.BranchName
	}
	if _, err := r.Provider.Commit(ctx, providers.CommitRequest{
		Repository: r.Config.Repository,
		Branch:     branchName,
		BaseSHA:    branch.SHA,
		Message:    proposal.Title,
		Files:      proposal.Files,
	}); err != nil {
		return RunResult{}, fmt.Errorf("commit tutor proposal: %w", err)
	}
	pr, err := r.Provider.OpenPullRequest(ctx, providers.PullRequestRequest{
		Repository: r.Config.Repository,
		Title:      proposal.Title,
		Body:       proposal.Body,
		Head:       branchName,
		Base:       baseBranch,
		Draft:      r.Config.Draft,
	})
	if err != nil {
		return RunResult{}, fmt.Errorf("open tutor proposal pull request: %w", err)
	}
	if len(r.Config.Reviewers) > 0 {
		if err := r.Provider.RequestReview(ctx, providers.ReviewRequest{
			Repository: r.Config.Repository,
			PullID:     prID(pr),
			Reviewers:  append([]string(nil), r.Config.Reviewers...),
		}); err != nil {
			return RunResult{}, fmt.Errorf("request tutor proposal review: %w", err)
		}
	}
	recorder.RecordProposal(ctx, proposal, pr)
	result.Proposal = &proposal
	result.PullRequest = &pr
	result.Opened = true
	return result, nil
}

func (r Runner) mergeWithExistingConfig(ctx context.Context, baseBranch string, proposal *Proposal) error {
	hasEdits := false
	for _, file := range proposal.Files {
		if file.ChangeType == string(providers.CommitChangeEdit) {
			hasEdits = true
			break
		}
	}
	if !hasEdits {
		return nil
	}
	dir, err := os.MkdirTemp("", "goobers-tutor-config-*")
	if err != nil {
		return fmt.Errorf("create tutor config checkout: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	clone, err := r.Provider.CloneRepository(ctx, providers.CloneRequest{
		Repository:  r.Config.Repository,
		Destination: dir,
		Branch:      baseBranch,
	})
	if err != nil {
		return fmt.Errorf("clone config repository: %w", err)
	}
	root := clone.Path
	if root == "" {
		root = dir
	}
	for i, file := range proposal.Files {
		if file.ChangeType != string(providers.CommitChangeEdit) {
			continue
		}
		existing, err := os.ReadFile(filepath.Join(root, filepath.Clean(file.Path)))
		if err != nil {
			return fmt.Errorf("read current config file %q: %w", file.Path, err)
		}
		merged, err := mergeProposalContent(string(existing), file, proposal.Finding)
		if err != nil {
			return fmt.Errorf("merge tutor proposal into %q: %w", file.Path, err)
		}
		proposal.Files[i].Content = merged
	}
	return nil
}

func mergeProposalContent(existing string, file providers.CommitFile, finding Finding) (string, error) {
	switch {
	case strings.HasSuffix(file.Path, "instructions.md"):
		return mergeInstructionContent(existing, file.Content), nil
	case strings.HasSuffix(file.Path, ".yaml"), strings.HasSuffix(file.Path, ".yml"):
		return mergeWorkflowContent(existing, file.Content, finding)
	default:
		return "", fmt.Errorf("unsupported edit target")
	}
}

func mergeInstructionContent(existing, guidance string) string {
	marker := tutorMarker(guidance)
	if marker != "" && strings.Contains(existing, marker) {
		return existing
	}
	trimmed := strings.TrimRight(existing, "\n")
	if trimmed == "" {
		return strings.TrimLeft(guidance, "\n")
	}
	return trimmed + "\n\n" + strings.TrimLeft(guidance, "\n")
}

func mergeWorkflowContent(existing, guidance string, finding Finding) (string, error) {
	var wf apiv1.Workflow
	if err := yaml.Unmarshal([]byte(existing), &wf); err != nil {
		return "", err
	}
	if wf.Annotations == nil {
		wf.Annotations = map[string]string{}
	}
	wf.Annotations["goobers.dev/tutor-finding"] = string(finding.Type)
	wf.Annotations["goobers.dev/tutor-target"] = targetLabel(finding)
	wf.Annotations["goobers.dev/tutor-rationale"] = finding.Rationale
	wf.Annotations["goobers.dev/tutor-recommendation"] = finding.Recommendation
	for i := range wf.Spec.Tasks {
		if wf.Spec.Tasks[i].Name != finding.TaskID {
			continue
		}
		if !strings.Contains(wf.Spec.Tasks[i].Goal, finding.Recommendation) {
			wf.Spec.Tasks[i].Goal = strings.TrimSpace(wf.Spec.Tasks[i].Goal + " Tutor guidance: " + finding.Recommendation)
		}
	}
	for i := range wf.Spec.Gates {
		if wf.Spec.Gates[i].Name != finding.GateID || wf.Spec.Gates[i].Automated == nil {
			continue
		}
		if wf.Spec.Gates[i].Automated.Params == nil {
			wf.Spec.Gates[i].Automated.Params = map[string]string{}
		}
		wf.Spec.Gates[i].Automated.Params["tutorRecommendation"] = finding.Recommendation
	}
	out, err := yaml.Marshal(wf)
	if err != nil {
		return "", err
	}
	if marker := tutorMarker(guidance); marker != "" {
		return string(out) + "\n# " + strings.TrimSpace(strings.ReplaceAll(marker, "\n", " ")) + "\n", nil
	}
	return string(out), nil
}

func tutorMarker(guidance string) string {
	for _, line := range strings.Split(guidance, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "<!-- goober-tutor:") {
			return line
		}
	}
	return ""
}
