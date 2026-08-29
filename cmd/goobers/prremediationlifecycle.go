package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/journal"
)

const prRemediationLifecycleResultFile = "pr-remediation-lifecycle.json"

const prRemediationLifecycleHelp = "Usage: goobers pr-claim [--release] [path]\n\n" +
	"At a pr-remediation stage boundary, verify that this run's claimed pull\n" +
	"request is still open. If it has merged or closed, release the claim and\n" +
	"return a structured no-work result so the runner stops the workflow.\n" +
	"With --release, explicitly release the run's PR claim without querying the\n" +
	"provider. Releasing an already-released claim is an idempotent success.\n\n" +
	"Exit codes: 0 = PR open, terminal no-work, or released; 1 = business error;\n" +
	"2 = usage/IO error.\n"

type prRemediationLifecycleResult struct {
	SelectedNumber string `json:"selectedNumber,omitempty"`
	Open           bool   `json:"open"`
	Released       bool   `json:"released"`
	NoWork         bool   `json:"noWork,omitempty"`
}

func runPRRemediationLifecycle(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("pr-claim", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "pr-claim")
	release := fs.Bool("release", false, "release this run's PR claim without checking provider state")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)

	number, held, err := claimedPullRequestNumber(root)
	if err != nil {
		return failProviderStage(stderr, "read remediation PR claim", err, prRemediationLifecycleResultFile)
	}
	if *release {
		if err := releasePRRemediationClaim(root); err != nil {
			return failProviderStage(stderr, "release remediation PR claim", err, prRemediationLifecycleResultFile)
		}
		result := prRemediationLifecycleResult{Released: held}
		if held {
			result.SelectedNumber = strconv.Itoa(number)
		}
		return writePRRemediationLifecycleResult(result, stdout, stderr)
	}
	if !held {
		return writePRRemediationLifecycleResult(prRemediationLifecycleResult{
			Released: true,
			NoWork:   true,
		}, stdout, stderr)
	}

	repo, err := providerRepo(root)
	if err != nil {
		return failProviderStage(stderr, "load remediation repository", err, prRemediationLifecycleResultFile)
	}
	token, err := providerToken(capability.GitHubPRWrite)
	if err != nil {
		return failProviderStage(stderr, "load remediation credential", err, prRemediationLifecycleResultFile)
	}
	provider, err := remediationStageProvider(root, repo, token, false)
	if err != nil {
		return failProviderStage(stderr, "initialize remediation provider", err, prRemediationLifecycleResultFile)
	}
	ctx, cancel := providerCommandContext()
	defer cancel()
	pr, err := provider.GetPullRequest(ctx, repo, strconv.Itoa(number))
	if err != nil {
		return failProviderStage(stderr, "refresh remediation pull request", err, prRemediationLifecycleResultFile)
	}
	if strings.EqualFold(pr.State, "open") && !pr.Merged {
		return writePRRemediationLifecycleResult(prRemediationLifecycleResult{
			SelectedNumber: strconv.Itoa(number),
			Open:           true,
		}, stdout, stderr)
	}

	if err := releasePRRemediationClaim(root); err != nil {
		return failProviderStage(stderr, "release terminal remediation PR claim", err, prRemediationLifecycleResultFile)
	}
	return writePRRemediationLifecycleResult(prRemediationLifecycleResult{
		SelectedNumber: strconv.Itoa(number),
		Released:       true,
		NoWork:         true,
	}, stdout, stderr)
}

func releasePRRemediationClaim(root string) error {
	runID, _, err := providerRunContext()
	if err != nil {
		return err
	}
	l := layoutFor(root)
	log, _, err := journal.OpenInstanceLog(l.SchedulerDir())
	if err != nil {
		return fmt.Errorf("open instance log: %w", err)
	}
	defer func() { _ = log.Close() }()
	return releasePullRequestClaimsForRun(l, log, runID)
}

func writePRRemediationLifecycleResult(result prRemediationLifecycleResult, stdout, stderr io.Writer) int {
	data, err := json.Marshal(result)
	if err != nil {
		pf(stderr, "error: marshal PR remediation lifecycle result: %v\n", err)
		return 2
	}
	resultFile := providerInput("resultFile", prRemediationLifecycleResultFile)
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 2
	}
	if result.NoWork {
		if result.SelectedNumber == "" {
			pln(stdout, "no work: this run holds no PR claim")
		} else {
			pf(stdout, "no work: this run's claimed PR #%s is no longer open; claim released\n", result.SelectedNumber)
		}
		return 0
	}
	if result.Released {
		if result.SelectedNumber == "" {
			pln(stdout, "PR claim already released; nothing to do")
		} else {
			pf(stdout, "released claim for PR #%s\n", result.SelectedNumber)
		}
		return 0
	}
	pf(stdout, "claimed PR #%s is still open\n", result.SelectedNumber)
	return 0
}
