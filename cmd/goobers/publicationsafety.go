package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/diagnostictmp"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/publicationguard"
)

// Bound both memory and execution time. An uninspectable publication is a
// refusal, never a clean scan. No raw git output is included in errors.
const publicationReadLimit = 32 << 20

var publicationCommitID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func checkPublicationSafety(ctx context.Context, dir, base, head string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	read := func(ctx context.Context, args ...string) ([]byte, error) {
		cmd := workspaceGitCommand(dir, args...)
		// workspaceGitCommand owns hardened config and environment; attach the
		// check's deadline without losing them.
		command := exec.CommandContext(ctx, cmd.Path, cmd.Args[1:]...)
		command.Dir, command.Env = cmd.Dir, append(cmd.Env, "GIT_NO_REPLACE_OBJECTS=1")
		command.Env = append(command.Env, "GIT_NO_LAZY_FETCH=1")
		out := &publicationBuffer{}
		command.Stdout = out
		if err := command.Run(); err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return nil, fmt.Errorf("local git inspection failed: %w", err)
		}
		return out.Bytes(), nil
	}
	for _, ref := range []string{"refs/remotes/origin/" + base, "refs/heads/" + base, base} {
		out, err := read(ctx, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
		if ctx.Err() != nil {
			return "", fmt.Errorf("publication safety: resolve base: %w", ctx.Err())
		}
		if sha := strings.TrimSpace(string(out)); err == nil && publicationCommitID.MatchString(sha) {
			return publicationguard.Check(ctx, read, sha, head)
		}
	}
	return "", fmt.Errorf("publication safety: cannot resolve base; fetch the base branch before publishing")
}

type publicationBuffer struct{ buf bytes.Buffer }

func (b *publicationBuffer) Bytes() []byte { return b.buf.Bytes() }
func (b *publicationBuffer) Len() int      { return b.buf.Len() }

func (b *publicationBuffer) Write(p []byte) (int, error) {
	if len(p) > publicationReadLimit-b.Len() {
		return 0, fmt.Errorf("publication inspection exceeds size limit")
	}
	return b.buf.Write(p)
}

// Diagnostic scopes use leases, so another live daemon retains its captures.
func sweepOrphanedDiagnostics(log *journal.InstanceLog) {
	_, err := diagnostictmp.Sweep("")
	newSweepErrorReporter(log, "diagnostic_tmp_sweep_failed").report(err)
}
