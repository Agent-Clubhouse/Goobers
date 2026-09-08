package recovery

import (
	"context"
	"fmt"
	"strings"
)

// VerifyRestoredCommit proves that a single-parent commit contains exactly the
// retained patch applied to its recorded parent. It does not infer provenance
// from a branch name or commit message. The caller must bind this commit to its
// receiving run's durable prepared result; this does not prove a fresh fetch.
// Verification replays the patch using a private index, leaving HEAD untouched.
func VerifyRestoredCommit(ctx context.Context, repository string, record Record, commit string, maxPatchBytes int64) error {
	if !gitObjectID.MatchString(commit) {
		return fmt.Errorf("restoration verification requires an exact commit")
	}
	var metadata snapshotPathOutput
	if err := recoveryGit(ctx, repository, &metadata, "show", "--no-patch", "--format=%T%n%P", commit); err != nil {
		return err
	}
	fields := strings.Fields(metadata.String())
	if len(fields) != 2 || !gitObjectID.MatchString(fields[0]) || !gitObjectID.MatchString(fields[1]) {
		return fmt.Errorf("prepared restoration must have exactly one parent")
	}
	tree, err := restoredSnapshotTree(ctx, repository, record, fields[1], maxPatchBytes)
	if err != nil {
		return err
	}
	if tree != fields[0] {
		return fmt.Errorf("prepared restoration does not match retained implementation")
	}
	return nil
}
