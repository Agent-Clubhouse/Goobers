package providers

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRepositoryProposalLostOrRejectedMutationNeverRetries(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderADO} {
		for _, mode := range []string{"lost", "server failure", "oversized"} {
			t.Run(string(kind)+"/"+mode, func(t *testing.T) {
				s, p := newProposalTestProvider(t, kind)
				s.mode = mode
				phase := "tree"
				if kind == ProviderADO {
					phase = "branch"
				}
				result, err := p.ApplyRepositoryProposalPhase(context.Background(), RepositoryProposalPhaseInput{Phase: phase, Proposal: s.proposal})
				if err == nil || !result.MutationAttempted || result.Acknowledged || len(s.posts) != 1 {
					t.Fatalf("mutation replayed or uncertainty lost: %+v %v posts=%v", result, err, s.posts)
				}
			})
		}
	}
}

func TestRepositoryProposalBadSourceNeverReachesMutation(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderADO} {
		for _, mode := range []string{"source moved", "symlink", "old blob", "foreign target", "missing marker", "default branch", "oversized content", "bad phase"} {
			t.Run(string(kind)+"/"+mode, func(t *testing.T) {
				s, p := newProposalTestProvider(t, kind)
				phase := "tree"
				if kind == ProviderADO {
					phase = "branch"
				}
				in := RepositoryProposalPhaseInput{Phase: phase, Proposal: s.proposal}
				switch mode {
				case "source moved", "symlink":
					s.mode = mode
				case "old blob":
					in.Proposal.PreviousBlob = strings.Repeat("f", 40)
				case "foreign target":
					in.Proposal.Repository.ID = "foreign-guid"
				case "missing marker":
					in.Proposal.Message = "unattributed"
				case "default branch":
					in.Proposal.BaseBranch = RepositoryProposalBranch(in.Proposal.CommandID)
				case "oversized content":
					in.Proposal.Content = make([]byte, MaxRepositorySourceBytes+1)
				case "bad phase":
					in.Phase = "update-pr"
				}
				result, err := p.ApplyRepositoryProposalPhase(context.Background(), in)
				if err == nil || result.MutationAttempted || len(s.posts) != 0 {
					t.Fatalf("unsafe effect %+v %v posts=%v", result, err, s.posts)
				}
			})
		}
	}
}

func TestRepositoryProposalExistingBranchIsNeverUpdated(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderADO} {
		t.Run(string(kind), func(t *testing.T) {
			s, p := newProposalTestProvider(t, kind)
			s.branch = s.proposal.BaseCommit
			in := RepositoryProposalPhaseInput{Phase: "branch", Proposal: s.proposal}
			if kind == ProviderGitHub {
				in.CommitID, in.TreeID = s.commit, s.tree
			}
			result, err := p.ApplyRepositoryProposalPhase(context.Background(), in)
			if err == nil || result.Acknowledged || len(s.posts) != 1 || s.branch != s.proposal.BaseCommit {
				t.Fatal("existing branch adopted/updated", result, err)
			}
		})
	}
}

func TestRepositoryProposalObservationRejectsAmbiguityAndChangedIntent(t *testing.T) {
	for _, kind := range []ProviderKind{ProviderGitHub, ProviderADO} {
		for _, mode := range []string{"ambiguous", "wrong title", "foreign repo", "wrong parent", "extra changes"} {
			if kind == ProviderGitHub && mode == "extra changes" {
				continue
			}
			t.Run(string(kind)+"/"+mode, func(t *testing.T) {
				s, p := newProposalTestProvider(t, kind)
				in := RepositoryProposalPhaseInput{Phase: "pull-request", Proposal: s.proposal, CommitID: s.commit}
				var pr map[string]interface{}
				if kind == ProviderGitHub {
					in.TreeID = s.tree
					pr = s.githubPR()
				} else {
					pr = s.adoPR()
				}
				s.prs = []map[string]interface{}{pr}
				switch mode {
				case "ambiguous":
					s.prs = append(s.prs, pr)
				case "wrong title":
					pr["title"] = "changed"
				case "foreign repo":
					if kind == ProviderGitHub {
						pr["head"].(map[string]interface{})["repo"] = map[string]string{"full_name": "other/repo"}
					} else {
						pr["repository"].(map[string]interface{})["name"] = "other"
					}
				case "wrong parent", "extra changes":
					in.Phase, s.mode = "commit", mode
				}
				observation, err := p.ObserveRepositoryProposalPhase(context.Background(), in)
				if !errors.Is(err, ErrRepositoryProposal) || observation.Matches || len(s.posts) != 0 {
					t.Fatal("unverified observation accepted", observation, err)
				}
			})
		}
	}
}
