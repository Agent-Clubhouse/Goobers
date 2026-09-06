package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
)

const cancelPendingCIHelp = "Usage: goobers cancel-pending-ci [path]\n\n" +
	"Cancel a bounded set of provider CI runs still pending for the exact\n" +
	"reviewed pull-request head. Requires pullNumber and headSha inputs.\n" +
	"Unsupported providers and provider failures are reported as explicit\n" +
	"non-fatal result states so a published review verdict is never hidden.\n"

type cancelPendingCIResult struct {
	Status     string   `json:"status"`
	Reason     string   `json:"reason,omitempty"`
	Examined   int      `json:"examined"`
	Canceled   []string `json:"canceled,omitempty"`
	Skipped    []string `json:"skipped,omitempty"`
	HeadSHA    string   `json:"headSha"`
	PullNumber string   `json:"pullNumber"`
}

func runCancelPendingCI(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("cancel-pending-ci", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "cancel-pending-ci")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}
	resultFile := providerInput("resultFile", "cancel-pending-ci-result.json")
	pullNumber := strings.TrimSpace(providerInput("pullNumber", ""))
	headSHA := strings.TrimSpace(providerInput("headSha", ""))
	if pullNumber == "" || headSHA == "" {
		pf(stderr, "error: pullNumber and headSha are required\n")
		return 1
	}
	maxRuns, err := strconv.Atoi(providerInput("maxRuns", "25"))
	if err != nil || maxRuns < 1 || maxRuns > 100 {
		pf(stderr, "error: maxRuns must be an integer between 1 and 100\n")
		return 1
	}
	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	stageProvider, err := newProviderForStage(
		root, repo, false,
		withStageProviderCapability(capability.ProviderCICancel),
		withStageProviderMutations("pr"),
	)
	if err != nil {
		return writeCancelPendingCIResult(resultFile, cancelPendingCIResult{
			Status: "failed", Reason: "provider setup failed", HeadSHA: headSHA, PullNumber: pullNumber,
		}, stdout, stderr)
	}
	ctx, cancel := providerCommandContext()
	defer cancel()
	result, err := providers.NewDispatcher(stageProvider).CancelPendingChecks(ctx, providers.CancelPendingChecksRequest{
		Repository: repo,
		PullID:     pullNumber,
		HeadSHA:    headSHA,
		Limit:      maxRuns,
	})
	if err != nil {
		var unsupported providers.ErrUnsupported
		var headMoved providers.PullRequestHeadMovedError
		status := "failed"
		reason := "provider cancellation failed"
		if errors.As(err, &unsupported) {
			status = "unsupported"
			reason = "provider does not support CI cancellation"
		} else if errors.As(err, &headMoved) {
			status = "stale-head"
			reason = "pull request head changed after review"
		} else if providers.IsAuthenticationError(err) {
			status = "unauthorized"
			reason = "credential cannot cancel provider CI"
		}
		return writeCancelPendingCIResult(resultFile, cancelPendingCIResult{
			Status: status, Reason: reason, HeadSHA: headSHA, PullNumber: pullNumber,
		}, stdout, stderr)
	}
	status := "completed"
	if len(result.Canceled) == 0 {
		status = "no-pending-checks"
	}
	return writeCancelPendingCIResult(resultFile, cancelPendingCIResult{
		Status: status, Examined: result.Examined, Canceled: result.Canceled,
		Skipped: result.Skipped, HeadSHA: headSHA, PullNumber: pullNumber,
	}, stdout, stderr)
}

func writeCancelPendingCIResult(path string, result cancelPendingCIResult, stdout, stderr io.Writer) int {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		pf(stderr, "error: encode pending-CI cancellation result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", path, err)
		return 1
	}
	switch result.Status {
	case "completed":
		pf(stdout, "canceled %d pending CI run(s) for PR #%s at %s\n", len(result.Canceled), result.PullNumber, result.HeadSHA)
	case "no-pending-checks":
		pf(stdout, "no pending CI runs remain for PR #%s at %s\n", result.PullNumber, result.HeadSHA)
	default:
		pf(stderr, "warning: pending CI cancellation %s for PR #%s at %s: %s\n", result.Status, result.PullNumber, result.HeadSHA, result.Reason)
	}
	return 0
}
