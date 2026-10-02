package main

import (
	"context"
	"io"
	"strconv"

	"github.com/goobers/goobers/providers"
)

type selectedPREnvelope struct {
	Number          int
	NumberString    string
	HeadSHA         string
	BaseSHA         string
	Advisory        bool
	ScopeGateParked string
}

type selectedPREnvelopeSource struct {
	numberSource string
	requireSHAs  bool
}

var (
	electLanderEnvelopeSource = selectedPREnvelopeSource{
		numberSource: "gather-sibling-context's selectedNumber output",
	}
	applyVerdictEnvelopeSource = selectedPREnvelopeSource{
		numberSource: "pr-select's number output",
		requireSHAs:  true,
	}
)

func readSelectedPREnvelope(stderr io.Writer, source selectedPREnvelopeSource) (selectedPREnvelope, int, bool) {
	numberString := providerInput("selectedNumber", "")
	if numberString == "" {
		pf(stderr, "error: selectedNumber is required (inputsFrom %s)\n", source.numberSource)
		return selectedPREnvelope{}, 1, false
	}
	number, err := strconv.Atoi(numberString)
	if err != nil {
		pf(stderr, "error: invalid selectedNumber %q: %v\n", numberString, err)
		return selectedPREnvelope{}, 1, false
	}
	headSHA := providerInput("selectedHeadSha", "")
	if source.requireSHAs && headSHA == "" {
		pf(stderr, "error: selectedHeadSha is required (inputsFrom gather-sibling-context's deterministic output)\n")
		return selectedPREnvelope{}, 1, false
	}
	baseSHA := providerInput("selectedBaseSha", "")
	if source.requireSHAs && baseSHA == "" {
		pf(stderr, "error: selectedBaseSha is required (inputsFrom gather-sibling-context's deterministic output)\n")
		return selectedPREnvelope{}, 1, false
	}
	advisory, err := strconv.ParseBool(providerInput("advisoryMode", "false"))
	if err != nil {
		pf(stderr, "error: invalid advisoryMode input: %v\n", err)
		return selectedPREnvelope{}, 1, false
	}
	return selectedPREnvelope{
		Number:          number,
		NumberString:    numberString,
		HeadSHA:         headSHA,
		BaseSHA:         baseSHA,
		Advisory:        advisory,
		ScopeGateParked: providerInput("scopeGateParked", ""),
	}, 0, true
}

func (e selectedPREnvelope) baseResult() map[string]string {
	return map[string]string{
		"selectedNumber":  strconv.Itoa(e.Number),
		"selectedHeadSha": e.HeadSHA,
		"selectedBaseSha": e.BaseSHA,
		"advisoryMode":    strconv.FormatBool(e.Advisory),
		"scopeGateParked": e.ScopeGateParked,
	}
}

type electionPRLister interface {
	ListPullRequests(context.Context, providers.ListPullRequestsRequest) ([]providers.PullRequestSummary, error)
}

type electionPRSource struct {
	lister            electionPRLister
	exclusionProvider remediationProvider
	resultFile        string
}

func listElectionPRs(ctx context.Context, source electionPRSource, repo providers.RepositoryRef, stderr io.Writer) ([]providers.PullRequestSummary, int, bool) {
	prs, err := source.lister.ListPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo,
		Base:       providerInput("base", providerBaseBranch()),
		HeadPrefix: providerInput("headPrefix", providerBranchNamespace()),
	})
	if err != nil {
		return nil, failProviderStage(stderr, "list pull requests", err, source.resultFile), false
	}
	return prs, 0, true
}

func resolveElectionPRExclusions(ctx context.Context, source electionPRSource, repo providers.RepositoryRef, prs []providers.PullRequestSummary, stderr io.Writer) (map[int]bool, int, bool) {
	if source.exclusionProvider == nil {
		return nil, 0, true
	}
	excluded, err := electionExcludedSet(
		ctx,
		source.exclusionProvider,
		repo,
		prs,
		providerInput("unlandableSiblings", ""),
		stderr,
	)
	if err != nil {
		return nil, failProviderStage(stderr, "resolve lander eligibility", err, source.resultFile), false
	}
	return excluded, 0, true
}
