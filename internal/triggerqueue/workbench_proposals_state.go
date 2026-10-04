package triggerqueue

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

func proposalPhaseNames(kind providers.ProviderKind) []string {
	if kind == providers.ProviderGitHub {
		return []string{"tree", "commit", "branch", "pull-request"}
	}
	return []string{"branch", "commit", "pull-request"}
}

// WorkbenchProposalPhaseInput derives only already-retained native receipt pins.
// An explicit new Claim must still precede applying the returned next phase.
func WorkbenchProposalPhaseInput(record WorkbenchProposal, index int) (providers.RepositoryProposalPhaseInput, error) {
	if record.Plan == nil || index < 0 {
		return providers.RepositoryProposalPhaseInput{}, ErrTransition
	}
	names := proposalPhaseNames(record.Plan.Native.Repository.Provider)
	if index >= len(names) {
		return providers.RepositoryProposalPhaseInput{}, ErrTransition
	}
	in := providers.RepositoryProposalPhaseInput{Phase: names[index], Proposal: record.Plan.Native}
	for i, phase := range record.Phases {
		if i > index {
			break
		}
		if phase.Result != nil && phase.Result.Acknowledged {
			if phase.Name == "tree" {
				in.TreeID = phase.Result.TreeID
			}
			if phase.Name == "commit" {
				in.CommitID = phase.Result.CommitID
				if in.Proposal.Repository.Provider == providers.ProviderGitHub {
					in.TreeID = phase.Result.TreeID
				}
			}
		}
		if phase.Name == "commit" && in.CommitID == "" {
			if observation, found := lastProposalObservation(record, i); found && observation.Result.Matches {
				in.CommitID = observation.Result.CommitID
			}
		}
	}
	return in, nil
}

func lastProposalObservation(record WorkbenchProposal, index int) (WorkbenchProposalObservation, bool) {
	for i := len(record.Observations) - 1; i >= 0; i-- {
		if record.Observations[i].Phase == index {
			return record.Observations[i], true
		}
	}
	return WorkbenchProposalObservation{}, false
}

func proposalPhaseSatisfied(r WorkbenchProposal, index int) bool {
	phase := r.Phases[index]
	if phase.Result != nil && phase.Result.Acknowledged {
		return true
	}
	// A matching base ref does not prove the uncertain ADO creation owned it.
	if r.Plan.Native.Repository.Provider == providers.ProviderADO && phase.Name == "branch" {
		return false
	}
	observation, found := lastProposalObservation(r, index)
	return found && observation.Result.Matches
}

func proposalHasVisibleEffect(r WorkbenchProposal) bool {
	for _, phase := range r.Phases {
		if (phase.Name == "branch" || phase.Name == "pull-request") && (phase.Result == nil || phase.Result.MutationAttempted) {
			return true
		}
	}
	return false
}

func proposalHistoryState(r WorkbenchProposal) string {
	if len(r.Phases) == 0 {
		if r.Plan == nil {
			return "accepted"
		}
		return "prepared"
	}
	index := len(r.Phases) - 1
	phase := r.Phases[index]
	if proposalPhaseSatisfied(r, index) {
		if phase.Name != "pull-request" {
			return "prepared"
		}
		if phase.Result != nil && phase.Result.Acknowledged {
			return "confirmed"
		}
		return "observed"
	}
	if phase.Result == nil {
		return "attempting"
	}
	if phase.Result.MutationAttempted {
		return "unknown"
	}
	if proposalHasVisibleEffect(r) {
		return "blocked"
	}
	return "not-applied"
}

func validateWorkbenchProposalState(r WorkbenchProposal) error {
	if len(r.Phases) > 4 || len(r.Observations) > MaxWorkbenchProposalObservations || r.OmittedObservations < 0 {
		return ErrTransition
	}
	if r.Plan == nil && len(r.Phases) > 0 {
		return ErrTransition
	}
	if r.Plan != nil && len(r.Phases) > len(proposalPhaseNames(r.Plan.Native.Repository.Provider)) {
		return ErrTransition
	}
	for i := range r.Phases {
		if validateProposalPhaseRecord(r, i) != nil {
			return ErrTransition
		}
	}
	for _, observation := range r.Observations {
		if validateProposalObservation(r, observation) != nil {
			return ErrTransition
		}
	}
	if r.CompletedAt != nil && r.CompletedAt.Before(r.AcceptedAt) {
		return ErrTransition
	}
	terminal := r.State == "confirmed" || r.State == "observed" || r.State == "not-applied"
	if (r.CompletedAt != nil) != terminal {
		return ErrTransition
	}
	derived := proposalHistoryState(r)
	if r.State == derived {
		return nil
	}
	// A host may stop an unclaimed next phase after current policy/preflight
	// refusal. Partial visible effects remain pinned as blocked custody.
	if derived == "accepted" || derived == "prepared" {
		if r.State == "not-applied" && !proposalHasVisibleEffect(r) {
			return nil
		}
		if r.State == "blocked" && proposalHasVisibleEffect(r) {
			return nil
		}
	}
	return ErrTransition
}

func proposalOutcome(result providers.RepositoryProposalPhaseResult) string {
	if result.Acknowledged {
		return "acknowledged"
	}
	if result.MutationAttempted {
		return "unknown"
	}
	return "not-attempted"
}

func validateProposalResult(r WorkbenchProposal, index int, result providers.RepositoryProposalPhaseResult) error {
	if !result.Acknowledged {
		if result.TreeID != "" || result.CommitID != "" || result.PullRequest != nil {
			return ErrTransition
		}
		return nil
	}
	if !result.MutationAttempted {
		return ErrTransition
	}
	copyRecord := r
	copyRecord.Phases = append([]WorkbenchProposalPhase(nil), r.Phases...)
	copyRecord.Phases[index].Result = nil
	in, err := WorkbenchProposalPhaseInput(copyRecord, index)
	if err != nil {
		return err
	}
	return validateProposalAcknowledgement(in, result)
}
func validateProposalAcknowledgement(in providers.RepositoryProposalPhaseInput, result providers.RepositoryProposalPhaseResult) error {
	switch in.Phase {
	case "tree":
		if !providers.ValidSourceCommit(result.TreeID) || result.CommitID != "" || result.PullRequest != nil {
			return ErrTransition
		}
	case "commit":
		if !providers.ValidSourceCommit(result.TreeID) || !providers.ValidSourceCommit(result.CommitID) || result.PullRequest != nil {
			return ErrTransition
		}
		if in.CommitID != "" && result.CommitID != in.CommitID {
			return ErrConflict
		}
		if in.Proposal.Repository.Provider == providers.ProviderGitHub && result.TreeID != in.TreeID {
			return ErrConflict
		}
	case "branch":
		expected := in.CommitID
		if in.Proposal.Repository.Provider == providers.ProviderADO {
			expected = in.Proposal.BaseCommit
		}
		if result.CommitID != expected || result.TreeID != "" || result.PullRequest != nil {
			return ErrConflict
		}
	case "pull-request":
		if result.CommitID != in.CommitID || result.TreeID != "" || !validProposalPR(in.Proposal.Repository, result.PullRequest) {
			return ErrTransition
		}
	default:
		return ErrTransition
	}
	return nil
}

func validateProposalObservation(r WorkbenchProposal, observation WorkbenchProposalObservation) error {
	if observation.Phase < 0 || observation.Phase >= len(r.Phases) || observation.At.Before(r.Phases[observation.Phase].ClaimedAt) {
		return ErrTransition
	}
	o := observation.Result
	if (o.CommitID != "" && !providers.ValidSourceCommit(o.CommitID)) || (o.TreeID != "" && !providers.ValidSourceCommit(o.TreeID)) || (!o.Found && (o.Matches || o.PullRequest != nil)) {
		return ErrTransition
	}
	if !o.Matches {
		if o.PullRequest != nil {
			return ErrTransition
		}
		return nil
	}
	in, err := proposalPhaseInputBeforeObservation(r, observation.Phase)
	if err != nil {
		return err
	}
	return validateProposalObservedMatch(in, o)
}
func validateProposalObservedMatch(in providers.RepositoryProposalPhaseInput, o providers.RepositoryProposalObservation) error {
	switch in.Phase {
	case "tree":
		if in.TreeID == "" || o.TreeID != in.TreeID || o.CommitID != "" || o.PullRequest != nil {
			return ErrTransition
		}
	case "commit":
		if !providers.ValidSourceCommit(o.CommitID) || o.PullRequest != nil || o.CommitID == in.Proposal.BaseCommit {
			return ErrTransition
		}
		if in.Proposal.Repository.Provider == providers.ProviderGitHub && (in.CommitID == "" || o.CommitID != in.CommitID) {
			return ErrConflict
		}
	case "branch":
		expected := in.CommitID
		if in.Proposal.Repository.Provider == providers.ProviderADO {
			expected = in.Proposal.BaseCommit
		}
		if o.CommitID != expected || o.PullRequest != nil {
			return ErrConflict
		}
	case "pull-request":
		if o.CommitID != in.CommitID || !validProposalPR(in.Proposal.Repository, o.PullRequest) {
			return ErrTransition
		}
	default:
		return ErrTransition
	}
	return nil
}

func proposalPhaseInputBeforeObservation(r WorkbenchProposal, index int) (providers.RepositoryProposalPhaseInput, error) {
	// Do not let the observation being validated supply its own expected commit.
	copyRecord := r
	copyRecord.Observations = nil
	for _, observation := range r.Observations {
		if observation.Phase < index {
			copyRecord.Observations = append(copyRecord.Observations, observation)
		}
	}
	return WorkbenchProposalPhaseInput(copyRecord, index)
}

func validProposalPR(repo providers.RepositoryRef, pr *providers.PullRequestResult) bool {
	if pr == nil || pr.Number <= 0 || pr.ID != strconv.Itoa(pr.Number) || len(pr.URL) > 2048 {
		return false
	}
	u, err := url.Parse(pr.URL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if repo.Provider == providers.ProviderGitHub {
		return u.Host == "github.com" && u.Path == "/"+repo.Owner+"/"+repo.Name+"/pull/"+pr.ID
	}
	return u.Host == "dev.azure.com" && u.Path == "/"+repo.Owner+"/"+repo.Project+"/_git/"+repo.Name+"/pullrequest/"+pr.ID && !strings.Contains(u.EscapedPath(), "%2F")
}

func validateProposalPhaseRecord(r WorkbenchProposal, index int) error {
	phase := r.Phases[index]
	if phase.Name != proposalPhaseNames(r.Plan.Native.Repository.Provider)[index] || phase.ClaimedAt.Before(r.AcceptedAt) {
		return ErrTransition
	}
	if index > 0 && !proposalSatisfiedBefore(r, index-1, phase.ClaimedAt) {
		return ErrTransition
	}
	if phase.Result == nil {
		if phase.FinishedAt != nil || phase.Outcome != "" {
			return ErrTransition
		}
		return nil
	}
	if phase.FinishedAt == nil || phase.FinishedAt.Before(phase.ClaimedAt) || validateProposalResult(r, index, *phase.Result) != nil || phase.Outcome != proposalOutcome(*phase.Result) {
		return ErrTransition
	}
	return nil
}
func proposalSatisfiedBefore(r WorkbenchProposal, index int, at time.Time) bool {
	phase := r.Phases[index]
	if phase.Result != nil && phase.Result.Acknowledged && phase.FinishedAt != nil && !phase.FinishedAt.After(at) {
		return true
	}
	if r.Plan.Native.Repository.Provider == providers.ProviderADO && phase.Name == "branch" {
		return false
	}
	for _, o := range r.Observations {
		if o.Phase == index && o.Result.Matches && !o.At.After(at) {
			return true
		}
	}
	return false
}
