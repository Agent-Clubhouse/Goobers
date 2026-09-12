package recovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/goobers/goobers/internal/gooberassets"
)

// ErrReservedRecoveryPath keeps archived runtime assets available for forensic
// inspection without promoting them into an operator's restored branch.
var ErrReservedRecoveryPath = errors.New("retained patch modifies reserved runtime assets")

func validateRestorePaths(ctx context.Context, repository string, record Record) error {
	err := recoveryGit(ctx, repository, io.Discard,
		"diff", "--quiet", "--no-ext-diff", "--no-textconv", "--no-renames", "--ignore-submodules=none",
		record.BaseSHA, record.SnapshotSHA, "--", ":(top,literal)"+gooberassets.WorkspaceDir)
	if err == nil {
		return nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
		return ErrReservedRecoveryPath
	}
	return fmt.Errorf("inspect reserved recovery paths: %w", err)
}
