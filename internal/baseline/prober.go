package baseline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// maxProbeOutput bounds each stream a probe keeps. It is generous on purpose:
// the run side derives its failure roster from its WHOLE output, so a probe
// that kept less would see a smaller roster and never match (#4477). A stream
// longer than this is kept by its tail and the result marked Truncated, which
// makes the roster incomplete and the comparison fail open.
const maxProbeOutput = 8 << 20

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
	// Exec runs command in dir and returns its two streams separately — as the
	// shell executor keeps them, so both halves of a comparison extract their
	// diagnostic from the same shape. Nil uses the default os/exec
	// implementation.
	Exec func(ctx context.Context, dir string, env, command []string) (stdout, stderr string, green bool, err error)
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
	stdout, stderr, green, err := run(ctx, dir, p.Env, command)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("baseline: run %q at %s: %w", CommandKey(command), short(target.BaseSHA), err)
	}
	stdout, cutOut := boundOutput(stdout)
	stderr, cutErr := boundOutput(stderr)
	return ProbeResult{Green: green, Output: stdout, Stderr: stderr, Truncated: cutOut || cutErr}, nil
}

// execCommand is the default runner: a non-zero exit is a measurement (the
// baseline is red), not an error; only a command that could not be run at all
// is an error, because that leaves the baseline unknown rather than red.
func execCommand(ctx context.Context, dir string, env, command []string) (string, string, bool, error) {
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.String(), stderr.String(), false, nil
	}
	return "", "", false, err
}

// boundOutput keeps at most maxProbeOutput bytes of output, from the tail, and
// reports whether it cut anything.
func boundOutput(output string) (string, bool) {
	if len(output) <= maxProbeOutput {
		return output, false
	}
	trimmed := output[len(output)-maxProbeOutput:]
	if index := strings.IndexByte(trimmed, '\n'); index >= 0 && index+1 < len(trimmed) {
		trimmed = trimmed[index+1:]
	}
	return trimmed, true
}
