package executor

import (
	"context"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/gitexclude"
	"github.com/goobers/goobers/internal/mutationsidecar"
)

// effectiveResultFile resolves the result file this stage will write: the
// declared InputResultFile, or — for a stage whose command IS the goobers CLI
// and whose subcommand carries a registry default — that default. The second
// return is non-empty only in the implicit case, because that is the one the
// stage environment has to be told about (the declared case already carries it
// in env.Inputs).
func effectiveResultFile(env apiv1.InvocationEnvelope, command []string) (resultFile, implicit string) {
	resultFile = stringInput(env, InputResultFile)
	if resultFile != "" {
		return resultFile, ""
	}
	if !StageInvokesGoobersCLI(command) || len(command) < 2 {
		return "", ""
	}
	if defaultResultFile, ok := ProviderStageResultFile(command[1]); ok {
		return defaultResultFile, defaultResultFile
	}
	return "", ""
}

// ExcludeStageArtifacts hides this stage's own throwaway outputs — the
// result file and the mutation sidecar — from git in the workspace, before the
// stage can write them.
//
// Both are harness bookkeeping written at the workspace root, untracked and
// unignored in the target repo, so `ls-files --others --exclude-standard`
// selected them for recovery snapshot capture: a one-line result-file write
// turned an abandoned preparation that carried NO agent-authored work into a
// non-empty patch, which sailed past the empty-diff guard in
// internal/recovery/retain.go and consumed a retention-floored inventory slot.
// Excluding them at capture source makes that patch genuinely empty, so the
// existing SkipEmpty guard discards it, rather than adding a second
// worthlessness predicate that could disagree with the first (#5119).
//
// A git exclude never applies to a tracked path, so a repository that
// legitimately commits a file of one of these names keeps it visible and
// keeps it captured.
//
// Best effort by design: a scratch (non-repo) workspace, a workspace whose git
// metadata is unreadable, or a host without git all leave the stage to run
// exactly as before. Failing a stage over an optimization that only affects how
// much recovery evidence is retained would trade a real outage for a
// hypothetical one.
func ExcludeStageArtifacts(ctx context.Context, workspace, resultFile string) {
	if workspace == "" {
		return
	}
	patterns := []gitexclude.Pattern{{Line: "/" + mutationsidecar.FileName}}
	if pattern, ok := gitexclude.Anchored(resultFile); ok {
		patterns = append(patterns, pattern)
	}
	_ = gitexclude.Ensure(ctx, workspace, patterns...)
}
