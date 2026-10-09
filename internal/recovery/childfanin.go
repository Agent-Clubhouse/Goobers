package recovery

import (
	"context"
	"fmt"
	"io"
	"time"
)

// PrepareChildFanIn combines immutable fork-relative results in declaration
// order using a private index. Every input is validated before combining. A
// conflict returns no prepared disposition and leaves live files, HEAD, index,
// and all input refs unchanged. The patch budget covers the complete fan-in.
//
// The coordinator must retain every input carrier, record preparation ownership
// before this call can pin the final disposition, and persist an application
// plan before applying it under exclusive root custody. This is not an applied
// receipt, and must not be regenerated after an interrupted application.
func PrepareChildFanIn(ctx context.Context, repository string, fork, parent ChildSnapshot, results []Record, operation string, at time.Time, maxBytes int64) (PreparedChildDisposition, error) {
	var empty PreparedChildDisposition
	if len(results) == 0 || len(results) > 128 {
		return empty, fmt.Errorf("child fan-in requires between 1 and 128 results")
	}
	for index, result := range results {
		if err := validateChildDisposition(fork, parent, result, ChildMerge, operation, at, maxBytes); err != nil {
			return empty, fmt.Errorf("result %d: %w", index+1, err)
		}
		if err := validateFanInResult(ctx, repository, fork, result); err != nil {
			return empty, fmt.Errorf("result %d: %w", index+1, err)
		}
	}
	if err := CheckChildSnapshotCurrent(ctx, repository, parent); err != nil {
		return empty, err
	}
	if err := recoveryGit(ctx, repository, io.Discard, "merge-base", "--is-ancestor", fork.Record.BaseSHA, parent.Record.BaseSHA); err != nil {
		return empty, fmt.Errorf("fan-in parent changed ancestry: %w", ErrIncompatibleSnapshot)
	}
	tree, err := applySnapshotTrees(ctx, repository, results, parent.Record.SnapshotSHA, maxBytes)
	if err != nil {
		return empty, err
	}
	inputs := make([]string, len(results))
	for index, result := range results {
		inputs[index] = result.SnapshotSHA
	}
	combined, err := commitChildResultTree(ctx, repository, tree, parent.Record.SnapshotSHA, operation, at, inputs)
	if err != nil {
		return empty, err
	}
	prepared, err := pinDispositionTree(ctx, repository, parent, combined, tree, ChildMerge, operation, at)
	if err != nil {
		return empty, err
	}
	return PreparedChildDisposition{Action: ChildMerge, ExpectedParent: parent, ChildSnapshotSHA: combined, Prepared: prepared, TreeSHA: tree}, nil
}

func validateFanInResult(ctx context.Context, repository string, fork ChildSnapshot, result Record) error {
	if err := recoveryGit(ctx, repository, io.Discard, "merge-base", "--is-ancestor", fork.Record.SnapshotSHA, result.SnapshotSHA); err != nil {
		return fmt.Errorf("fan-in result changed ancestry: %w", ErrIncompatibleSnapshot)
	}
	if err := verifySnapshotPolicy(ctx, repository, result.SnapshotSHA, fork.Policy); err != nil {
		return err
	}
	return validateRestorePaths(ctx, repository, result)
}
