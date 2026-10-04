package worktree

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/platform/proc"
)

// Human Git operations use the existing owned-process-tree acknowledgement.
// Ordinary operations preserve their previous execution path.
func executeGitCommand(ctx context.Context, cmd *exec.Cmd, mode gitOutputMode) ([]byte, error) {
	if _, ok := ctx.Value(gitExecutionKey{}).(GitExecution); !ok {
		if mode == gitRawOutput {
			return cmd.Output()
		}
		return cmd.CombinedOutput()
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	tree, err := proc.Start(cmd)
	if err != nil {
		return nil, err
	}
	acknowledge := invoke.RegisterWorkspaceWriter(ctx)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var output []byte
	joined := false
	select {
	case err = <-done:
		joined = true
		output = append([]byte(nil), stdout.Bytes()...)
		if mode == gitCombinedOutput || err != nil {
			output = append(output, stderr.Bytes()...)
		}
	case <-ctx.Done():
		err = ctx.Err()
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	stopped := tree.StopAndWait(cleanup)
	if !joined {
		select {
		case <-done:
		case <-cleanup.Done():
			stopped = errors.Join(stopped, cleanup.Err())
		}
	}
	if acknowledge != nil {
		acknowledge(stopped)
	}
	return output, errors.Join(err, stopped)
}
