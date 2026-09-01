package baseline

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// maxProbeOutput bounds the failure evidence a probe keeps. The signature is
// derived from the tail, where a test runner prints its failure summary.
const maxProbeOutput = 16 << 10

// Checkout materializes a repository's target branch at a pinned commit in a
// disposable directory. It is the repository seam CommandProber runs in; the
// daemon backs it with the worktree manager, tests with a fixture directory.
type Checkout interface {
	// Materialize returns the directory holding target at its base SHA plus a
	// release function the caller always invokes when it is done with it.
	Materialize(ctx context.Context, target ProbeTarget) (dir string, release func(), err error)
}

// CommandProber measures a baseline by running the CI command itself against a
// disposable checkout of the target branch at the pinned base SHA — the same
// command, on the same commit the affected branch synced, so a matching
// failure is evidence about the base rather than an inference.
type CommandProber struct {
	// Checkout provides the base-pinned directory. Required.
	Checkout Checkout
	// Env is the environment the command runs with. Empty inherits the
	// daemon's, matching how the local-ci stage itself runs.
	Env []string
	// Exec runs command in dir. Nil uses the default os/exec implementation.
	Exec func(ctx context.Context, dir string, env, command []string) (output string, green bool, err error)
}

// Probe implements Prober.
func (p *CommandProber) Probe(ctx context.Context, target ProbeTarget, command []string) (ProbeResult, error) {
	if p == nil || p.Checkout == nil {
		return ProbeResult{}, fmt.Errorf("baseline: prober requires a checkout")
	}
	if len(command) == 0 {
		return ProbeResult{}, fmt.Errorf("baseline: prober requires a command")
	}
	dir, release, err := p.Checkout.Materialize(ctx, target)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("baseline: materialize %s at %s: %w", target.Repo, short(target.BaseSHA), err)
	}
	defer release()

	run := p.Exec
	if run == nil {
		run = execCommand
	}
	output, green, err := run(ctx, dir, p.Env, command)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("baseline: run %q at %s: %w", CommandKey(command), short(target.BaseSHA), err)
	}
	return ProbeResult{Green: green, Output: boundOutput(output)}, nil
}

// execCommand is the default runner: a non-zero exit is a measurement (the
// baseline is red), not an error; only a command that could not be run at all
// is an error, because that leaves the baseline unknown rather than red.
func execCommand(ctx context.Context, dir string, env, command []string) (string, bool, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err == nil {
		return string(output), true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(output), false, nil
	}
	return "", false, err
}

func boundOutput(output string) string {
	if len(output) <= maxProbeOutput {
		return output
	}
	trimmed := output[len(output)-maxProbeOutput:]
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 && index+1 < len(trimmed) {
		trimmed = trimmed[index+1:]
	}
	return trimmed
}
