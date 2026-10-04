package providers

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxRepositoryProposalPhaseTime bounds one claimed phase or observation. The
// interactive service may impose an earlier aggregate deadline across phases.
const MaxRepositoryProposalPhaseTime = 5 * time.Second

// RepositoryProposalWriter performs one explicitly claimed remote phase. The
// caller must retain intent before calling and its receipt before the next phase.
// Observation never creates, updates, retries or adopts an existing branch/PR.
type RepositoryProposalWriter interface {
	ApplyRepositoryProposalPhase(context.Context, RepositoryProposalPhaseInput) (RepositoryProposalPhaseResult, error)
	ObserveRepositoryProposalPhase(context.Context, RepositoryProposalPhaseInput) (RepositoryProposalObservation, error)
}

// RepositoryProposal is immutable command custody, supplied by an authorized
// host. Provider clients carry exactly the selected interactive credential.
// A source edit is one existing regular file and always produces a draft PR.
type RepositoryProposal struct {
	Repository                 RepositoryRef
	CommandID, OperationDigest string
	BaseBranch, BaseCommit     string
	Path, PreviousBlob         string
	Content                    []byte
	Message, Title, Body       string
}

// RepositoryProposalPhaseInput selects one native mutation. GitHub orders tree,
// commit, branch, pull-request; ADO orders branch, commit, pull-request. Pins must
// come from prior durable receipts, never from the interactive request body.
type RepositoryProposalPhaseInput struct {
	Phase            string
	Proposal         RepositoryProposal
	TreeID, CommitID string
}

// RepositoryProposalPhaseResult keeps transport acknowledgement distinct from
// later read-only convergence. MutationAttempted is set before sending the POST;
// an error after that point never authorizes an automatic retry.
type RepositoryProposalPhaseResult struct {
	MutationAttempted, Acknowledged bool
	TreeID, CommitID                string
	PullRequest                     *PullRequestResult
}

// RepositoryProposalObservation describes observed state, not causal proof that
// this command authored it. Matching a base-only ADO branch cannot confer branch
// ownership after an uncertain create acknowledgement.
type RepositoryProposalObservation struct {
	Found, Matches   bool
	TreeID, CommitID string
	PullRequest      *PullRequestResult
}

// ErrRepositoryProposal refuses invalid scope, unsupported phases or mismatched
// provider evidence. Callers must retain attempted effects as uncertain.
var ErrRepositoryProposal = errors.New("provider: repository proposal is invalid or unverifiable")

// RepositoryProposalBranch derives one immutable branch from command identity.
// It returns empty for an invalid ID, never a configurable/default branch.
func RepositoryProposalBranch(commandID string) string {
	if !proposalHex(commandID, 32) {
		return ""
	}
	return "goobers/workbench/" + commandID
}

// RepositoryProposalMarker attributes source effects to exact retained command
// and operation identity without claiming a workflow run or exposing credentials.
func RepositoryProposalMarker(commandID, digest string) string {
	if !proposalHex(commandID, 32) || !proposalHex(digest, 64) {
		return ""
	}
	return "goobers-workbench:" + commandID + ":" + digest
}

func validateRepositoryProposal(input RepositoryProposalPhaseInput, kind ProviderKind) error {
	p := input.Proposal
	if p.Repository.Provider != kind || p.Repository.URL != "" || p.Repository.ID != "" || !proposalComponent(p.Repository.Owner) || !proposalComponent(p.Repository.Name) {
		return ErrRepositoryProposal
	}
	if (kind == ProviderGitHub && p.Repository.Project != "") || (kind == ProviderADO && !proposalComponent(p.Repository.Project)) {
		return ErrRepositoryProposal
	}
	if !sourceBranchName(p.BaseBranch) || !ValidSourceCommit(p.BaseCommit) || !ValidSourceCommit(p.PreviousBlob) || !ValidRepositorySourcePath(p.Path) || len(p.Content) > MaxRepositorySourceBytes || !utf8.Valid(p.Content) {
		return ErrRepositoryProposal
	}
	marker := RepositoryProposalMarker(p.CommandID, p.OperationDigest)
	if marker == "" || RepositoryProposalBranch(p.CommandID) == p.BaseBranch || !nativeEditText(p.Message, 4096, false) || !nativeEditText(p.Title, 256, false) || !nativeEditText(p.Body, 4000, false) || strings.Count(p.Message, marker) != 1 || strings.Count(p.Body, marker) != 1 {
		return ErrRepositoryProposal
	}
	if strings.ContainsAny(p.Title, "\r\n") {
		return ErrRepositoryProposal
	}
	return validateProposalPhase(input, kind)
}

func proposalComponent(value string) bool {
	return nativeEditText(value, 256, false) && strings.TrimSpace(value) == value && value != "." && value != ".." && !strings.ContainsAny(value, "/\\?#\r\n")
}

func validateProposalPhase(in RepositoryProposalPhaseInput, kind ProviderKind) error {
	switch in.Phase {
	case "tree":
		if kind != ProviderGitHub || (in.TreeID != "" && !ValidSourceCommit(in.TreeID)) || in.CommitID != "" {
			return ErrRepositoryProposal
		}
	case "commit":
		if kind == ProviderGitHub && !ValidSourceCommit(in.TreeID) {
			return ErrRepositoryProposal
		}
		if kind == ProviderADO && in.TreeID != "" {
			return ErrRepositoryProposal
		}
		if in.CommitID != "" && !ValidSourceCommit(in.CommitID) {
			return ErrRepositoryProposal
		}
	case "branch":
		if kind == ProviderGitHub && (!ValidSourceCommit(in.CommitID) || !ValidSourceCommit(in.TreeID)) {
			return ErrRepositoryProposal
		}
		if kind == ProviderADO && (in.TreeID != "" || in.CommitID != "") {
			return ErrRepositoryProposal
		}
	case "pull-request":
		if !ValidSourceCommit(in.CommitID) {
			return ErrRepositoryProposal
		}
		if kind == ProviderGitHub && !ValidSourceCommit(in.TreeID) {
			return ErrRepositoryProposal
		}
	default:
		return ErrRepositoryProposal
	}
	return nil
}

func proposalHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func proposalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(WithoutMutationRetries(WithResponseBodyLimit(ctx, MaxRepositorySourceResponseBytes)), MaxRepositoryProposalPhaseTime)
}

func proposalSourceMatches(ctx context.Context, reader RepositorySourceReader, proposal RepositoryProposal) error {
	head, err := reader.ReadSourceBranch(ctx, proposal.Repository, proposal.BaseBranch)
	if err != nil {
		return err
	}
	if head != proposal.BaseCommit {
		return ErrRepositoryProposal
	}
	file, err := reader.ReadRepositorySource(ctx, proposal.Repository, proposal.Path, proposal.BaseCommit)
	if err != nil {
		return err
	}
	if file.BlobID != proposal.PreviousBlob {
		return ErrRepositoryProposal
	}
	return nil
}

func proposalContentMatches(ctx context.Context, reader RepositorySourceReader, proposal RepositoryProposal, commit string) error {
	file, err := reader.ReadRepositorySource(ctx, proposal.Repository, proposal.Path, commit)
	if err != nil {
		return err
	}
	if file.BlobID != sourceBlobID(proposal.Content) || string(file.Content) != string(proposal.Content) {
		return ErrRepositoryProposal
	}
	return nil
}
