package recovery

import (
	"context"
	"fmt"
	"io"
	"os"
)

const (
	maxChildApplyPaths = 1024
	maxChildApplyBytes = 16 << 20
)

// ChildApplyPlan is persisted by the coordinator before any live mutation.
// Child application stages only paths changed by the disposition and preserves
// HEAD. Parent application additionally restores independent Git state. Both
// preserve unrelated staged entries and excluded runtime/credential paths.
type ChildApplyPlan struct {
	Version            int                      `json:"version"`
	Disposition        PreparedChildDisposition `json:"disposition"`
	AppliedIndexDigest string                   `json:"appliedIndexDigest"`
	Parent             *ParentApplication       `json:"parent,omitempty"`
}

// PlanChildApplication binds the exact before/after index and verifies bounded
// path content while the caller holds exclusive, acknowledged workspace custody.
// File/directory shape changes are refused in v1; they require a richer durable
// move protocol. Ordinary additions, removals, modes and safe symlinks work.
func PlanChildApplication(ctx context.Context, repository string, prepared PreparedChildDisposition) (ChildApplyPlan, error) {
	if err := VerifyChildDisposition(ctx, repository, prepared); err != nil {
		return ChildApplyPlan{}, err
	}
	changes, err := loadChildChanges(ctx, repository, prepared)
	if err != nil {
		return ChildApplyPlan{}, err
	}
	root, err := os.OpenRoot(repository)
	if err != nil {
		return ChildApplyPlan{}, err
	}
	defer func() { _ = root.Close() }()
	if err := checkChildFiles(root, changes, false); err != nil {
		return ChildApplyPlan{}, err
	}
	index, err := planChildIndex(ctx, repository, prepared.ExpectedParent.Record.BaseSHA, changes)
	if err != nil {
		return ChildApplyPlan{}, err
	}
	return ChildApplyPlan{Version: 1, Disposition: prepared, AppliedIndexDigest: index}, nil
}

// ApplyChildApplication requires the same exclusive custody as planning. A
// caller must first durably store plan, then retry this exact plan after crash.
// Each file is atomically before or after; the real index update is atomic too.
// Any third state refuses recovery rather than overwriting intervening work.
func ApplyChildApplication(ctx context.Context, repository string, plan ChildApplyPlan) error {
	if plan.Version != 1 || !patchDigest.MatchString(plan.AppliedIndexDigest) {
		return fmt.Errorf("invalid child application plan")
	}
	p := plan.Disposition
	if err := verifyPreparedDisposition(ctx, repository, p); err != nil {
		return err
	}
	head, index, err := workspaceSnapshotIdentity(ctx, repository)
	if err != nil {
		return err
	}
	finalHead, err := applicationHead(ctx, repository, plan)
	if err != nil {
		return err
	}
	if (head != p.ExpectedParent.Record.BaseSHA && head != finalHead) || (index != p.ExpectedParent.IndexDigest && index != plan.AppliedIndexDigest) {
		return ErrWorkspaceChanged
	}
	changes, err := loadChildChanges(ctx, repository, p)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(repository)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := checkChildFiles(root, changes, true); err != nil {
		return err
	}
	if err := checkUnrelatedChildFiles(ctx, repository, p, changes); err != nil {
		return err
	}
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeChildFile(root, change, p.Prepared.SnapshotSHA); err != nil {
			return err
		}
	}
	if err := applyApplicationIndex(ctx, repository, plan, changes); err != nil {
		return err
	}
	if head != finalHead {
		if err := recoveryGit(ctx, repository, io.Discard, "update-ref", "HEAD", finalHead, head); err != nil {
			return err
		}
	}
	return VerifyChildApplication(ctx, repository, plan)
}

// VerifyChildApplication is the coordinator's required acknowledgement gate.
func VerifyChildApplication(ctx context.Context, repository string, plan ChildApplyPlan) error {
	if plan.Version != 1 || !patchDigest.MatchString(plan.AppliedIndexDigest) {
		return fmt.Errorf("invalid child application plan")
	}
	p := plan.Disposition
	if err := verifyPreparedDisposition(ctx, repository, p); err != nil {
		return err
	}
	head, index, err := workspaceSnapshotIdentity(ctx, repository)
	if err != nil {
		return err
	}
	finalHead, err := applicationHead(ctx, repository, plan)
	if err != nil {
		return err
	}
	if head != finalHead || index != plan.AppliedIndexDigest {
		return ErrWorkspaceChanged
	}
	current, err := CaptureChildSnapshot(ctx, repository, p.ExpectedParent.Record.RepositoryKey, p.ExpectedParent.Record.RunID, p.ExpectedParent.Record.CreatedAt, p.ExpectedParent.Record.RetainUntil, p.ExpectedParent.Policy)
	if err != nil {
		return err
	}
	if current.TreeSHA != p.TreeSHA {
		return ErrWorkspaceChanged
	}
	return nil
}
