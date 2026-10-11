package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/instance"
)

const maxPRDescriptionPatchBytes = 64 * 1024

func observePRDescriptionShadow(root, runID, head, base, title, body string, inRepoDir func(func() error) error, stderr io.Writer) string {
	cfg, err := instance.LoadConfig(layoutFor(root).ConfigFile())
	if err != nil {
		pf(stderr, "warning: decisionGate PR description shadow disabled: load config: %v\n", err)
		return body
	}
	if cfg.DecisionGate.EffectiveMode() != decisiongate.ModeShadow ||
		!decisiongate.Sampled(runID, cfg.DecisionGate.ShadowSample) {
		return body
	}
	gate, err := cfg.DecisionGate.Resolve(nil, nil)
	if err != nil {
		pf(stderr, "warning: decisionGate PR description shadow disabled: %v\n", err)
		return body
	}

	state := decisiongate.PRDescriptionState{Title: title, Description: body, SizeBucket: "unknown"}
	if err := inRepoDir(func() error {
		var factsErr error
		state, factsErr = prDescriptionState(base, title, body)
		return factsErr
	}); err != nil {
		logPRDescriptionShadow(stderr, runID, head, base, state, decisiongate.Outcome{
			Name:     decisiongate.PRDescriptionAgreementQuestion,
			Decision: decisiongate.Uncertain,
		}, err)
		return body
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	outcome, scoreErr := gate.EvaluatePRDescription(ctx, state)
	logPRDescriptionShadow(stderr, runID, head, base, state, outcome, scoreErr)
	if scoreErr != nil || outcome.Decision == decisiongate.Uncertain {
		return body
	}
	return appendPRDescriptionReviewNote(body, state, outcome)
}

func prDescriptionState(base, title, body string) (decisiongate.PRDescriptionState, error) {
	state := decisiongate.PRDescriptionState{Title: title, Description: body, SizeBucket: "unknown"}
	numstat, err := gitDiffOutput(base, "--numstat")
	if err != nil {
		return state, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(numstat))
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), "\t", 3)
		if len(fields) != 3 {
			return state, fmt.Errorf("parse git diff --numstat line %q", scanner.Text())
		}
		state.ChangedFiles = append(state.ChangedFiles, fields[2])
		if fields[0] == "-" || fields[1] == "-" {
			state.BinaryFiles++
			continue
		}
		added, addErr := strconv.Atoi(fields[0])
		deleted, deleteErr := strconv.Atoi(fields[1])
		if addErr != nil || deleteErr != nil {
			return state, fmt.Errorf("parse git diff --numstat counts %q", scanner.Text())
		}
		state.AddedLines += added
		state.DeletedLines += deleted
	}
	if err := scanner.Err(); err != nil {
		return state, fmt.Errorf("read git diff --numstat: %w", err)
	}
	state.SizeBucket = prDiffSizeBucket(len(state.ChangedFiles), state.AddedLines+state.DeletedLines)

	patch, err := gitDiffOutput(base, "--no-ext-diff", "--unified=3")
	if err != nil {
		return state, err
	}
	if len(patch) > maxPRDescriptionPatchBytes {
		patch = patch[:maxPRDescriptionPatchBytes]
		state.PatchTruncated = true
	}
	state.Patch = string(patch)
	return state, nil
}

func gitDiffOutput(base string, args ...string) ([]byte, error) {
	gitArgs := append([]string{"diff", "--no-renames"}, args...)
	gitArgs = append(gitArgs, base+"...HEAD", "--")
	cmd := exec.Command("git", gitArgs...)
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
		return nil, fmt.Errorf("git diff: %s: %w", strings.TrimSpace(string(exitErr.Stderr)), err)
	}
	return nil, fmt.Errorf("git diff: %w", err)
}

func prDiffSizeBucket(files, changedLines int) string {
	switch {
	case files == 0:
		return "empty"
	case changedLines <= 50:
		return "1-50"
	case changedLines <= 500:
		return "51-500"
	default:
		return "501+"
	}
}

func logPRDescriptionShadow(stderr io.Writer, runID, head, base string, state decisiongate.PRDescriptionState, outcome decisiongate.Outcome, err error) {
	pf(stderr, "decisiongate.shadow question=%q runID=%q head=%q base=%q sizeBucket=%q files=%d changedLines=%d verdict=%q probability=%g cached=%t error=%q\n",
		decisiongate.PRDescriptionAgreementQuestion, runID, head, base, state.SizeBucket,
		len(state.ChangedFiles), state.AddedLines+state.DeletedLines, outcome.Decision,
		outcome.Probability, outcome.Cached, errString(err))
}

func appendPRDescriptionReviewNote(body string, state decisiongate.PRDescriptionState, outcome decisiongate.Outcome) string {
	note := fmt.Sprintf(
		"> **Goobers decision gate (shadow):** description/diff agreement `%s` (score %.3f; diff size `%s`). Advisory only.",
		outcome.Decision, outcome.Probability, state.SizeBucket,
	)
	return strings.TrimRight(body, "\n") + "\n\n" + note
}
