package recovery

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
)

// ChildDisposition selects a prepared parent working-state transition.
// Preparation never modifies the live parent checkout or its real index.
type ChildDisposition string

// Child workspace disposition actions are explicit parent choices.
const (
	ChildMerge   ChildDisposition = "merge"
	ChildReplace ChildDisposition = "replace"
	ChildDiscard ChildDisposition = "discard"
)

// PreparedChildDisposition is durable Git evidence of a proposed working tree,
// NOT an applied receipt. The production coordinator must persist intent, hold
// exclusive custody, call CheckChildSnapshotCurrent immediately before applying
// allowed paths, and record/verify the actual result after crash-safe application.
// Policy exclusions must remain untouched by that application, including replace.
type PreparedChildDisposition struct {
	Action           ChildDisposition `json:"action"`
	ExpectedParent   ChildSnapshot    `json:"expectedParent"`
	ChildSnapshotSHA string           `json:"childSnapshotSha"`
	Prepared         Record           `json:"prepared"`
	TreeSHA          string           `json:"treeSha"`
}

// PrepareChildDisposition uses the recovery private-index three-way engine to
// prepare merge, or an exact verified child tree for replace. Discard preserves
// the expected parent's permitted tree. All paths pin a reproducible commit
// with compare-and-swap; identical retries reuse it and conflicting keys refuse.
// The child result must descend from the exact recorded fork snapshot. No merge,
// replace, or discard here deletes provider-side effects or the child workspace.
func PrepareChildDisposition(ctx context.Context, repository string, fork, expectedParent ChildSnapshot, result Record, action ChildDisposition, operationID string, identityTime time.Time, maxPatchBytes int64) (PreparedChildDisposition, error) {
	if err := validateChildDisposition(fork, expectedParent, result, action, operationID, identityTime, maxPatchBytes); err != nil {
		return PreparedChildDisposition{}, err
	}
	if err := CheckChildSnapshotCurrent(ctx, repository, expectedParent); err != nil {
		return PreparedChildDisposition{}, err
	}
	if err := PinCommit(ctx, repository, result); err != nil {
		return PreparedChildDisposition{}, err
	}
	if err := verifySnapshotPolicy(ctx, repository, result.SnapshotSHA, fork.Policy); err != nil {
		return PreparedChildDisposition{}, err
	}
	tree, err := childDispositionTree(ctx, repository, fork, expectedParent, result, action, maxPatchBytes)
	if err != nil {
		return PreparedChildDisposition{}, err
	}
	prepared, err := pinDispositionTree(ctx, repository, expectedParent, result.SnapshotSHA, tree, action, operationID, identityTime)
	if err != nil {
		return PreparedChildDisposition{}, err
	}
	return PreparedChildDisposition{Action: action, ExpectedParent: expectedParent, ChildSnapshotSHA: result.SnapshotSHA, Prepared: prepared, TreeSHA: tree}, nil
}

func validateChildDisposition(fork, parent ChildSnapshot, result Record, action ChildDisposition, operationID string, identityTime time.Time, maxPatchBytes int64) error {
	if err := fork.validate(); err != nil {
		return err
	}
	if err := parent.validate(); err != nil {
		return err
	}
	if action != ChildMerge && action != ChildReplace && action != ChildDiscard {
		return fmt.Errorf("unknown child workspace disposition")
	}
	if _, err := RefForRun(operationID); err != nil {
		return err
	}
	if identityTime.IsZero() || !parent.Record.RetainUntil.After(identityTime) || maxPatchBytes <= 0 {
		return fmt.Errorf("child disposition requires stable identity, retention and positive patch budget")
	}
	if result.RepositoryKey != fork.Record.RepositoryKey || parent.Record.RepositoryKey != fork.Record.RepositoryKey || result.BaseSHA != fork.Record.SnapshotSHA {
		return fmt.Errorf("child disposition lineage does not match recorded fork")
	}
	if !reflect.DeepEqual(parent.Policy, fork.Policy) {
		return fmt.Errorf("child disposition changed the snapshot exclusion policy")
	}
	return result.validateRestorable()
}

func childDispositionTree(ctx context.Context, repository string, fork, parent ChildSnapshot, result Record, action ChildDisposition, maxPatchBytes int64) (string, error) {
	switch action {
	case ChildMerge:
		// Fork and current-parent snapshots are synthetic siblings, so the
		// recovery-to-main ancestor check does not describe their relationship.
		// Keep the real HEAD ancestry guard, and apply the exact fork-to-child
		// patch to the verified current parent using the shared private index.
		if err := recoveryGit(ctx, repository, io.Discard, "merge-base", "--is-ancestor", fork.Record.BaseSHA, parent.Record.BaseSHA); err != nil {
			return "", fmt.Errorf("child parent no longer descends from its recorded base: %w", ErrIncompatibleSnapshot)
		}
		if err := validateRestorePaths(ctx, repository, result); err != nil {
			return "", err
		}
		return applySnapshotTree(ctx, repository, result, parent.Record.SnapshotSHA, maxPatchBytes)
	case ChildReplace:
		return snapshotObject(ctx, repository, result.SnapshotSHA+"^{tree}")
	case ChildDiscard:
		return parent.TreeSHA, nil
	default:
		return "", fmt.Errorf("unknown child workspace disposition")
	}
}

func pinDispositionTree(ctx context.Context, repository string, parent ChildSnapshot, childSnapshot, tree string, action ChildDisposition, operationID string, identityTime time.Time) (Record, error) {
	// Bind the complete request identity into the commit; Git dates alone lose
	// sub-second precision. Nothing is written through the parent branch.
	date := identityTime.UTC().Format(time.RFC3339)
	environment := []string{"GIT_AUTHOR_NAME=Goobers Recovery", "GIT_AUTHOR_EMAIL=recovery@goobers.invalid", "GIT_COMMITTER_NAME=Goobers Recovery", "GIT_COMMITTER_EMAIL=recovery@goobers.invalid", "GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	message := "Prepare child disposition " + string(action) + " for " + operationID + "\n\nCapture identity: " + identityTime.UTC().Format(time.RFC3339Nano) + "\nChild: " + childSnapshot + "\nParent index: " + parent.IndexDigest
	var output boundedRefOutput
	if err := recoveryGitWithEnv(ctx, repository, &output, environment, "-c", "commit.gpgsign=false", "commit-tree", tree, "-p", parent.Record.SnapshotSHA, "-m", message); err != nil {
		return Record{}, err
	}
	commit := strings.TrimSpace(output.String())
	if !gitObjectID.MatchString(commit) {
		return Record{}, fmt.Errorf("invalid prepared disposition commit")
	}
	// Run-only pin makes the operation ID a CAS key: changing the intended tree
	// under the same key is a conflict, rather than another snapshot ref.
	ref, err := RefForRun(operationID)
	if err != nil {
		return Record{}, err
	}
	digest, err := WriteSnapshotPatch(ctx, repository, parent.Record.SnapshotSHA, commit, io.Discard)
	if err != nil {
		return Record{}, err
	}
	record := Record{Version: 1, RunID: operationID, RepositoryKey: parent.Record.RepositoryKey, Ref: ref, BaseSHA: parent.Record.SnapshotSHA, SnapshotSHA: commit, PatchDigest: digest, CreatedAt: identityTime, RetainUntil: parent.Record.RetainUntil}
	if err := PinCommit(ctx, repository, record); err != nil {
		return Record{}, err
	}
	return record, nil
}

// VerifyChildDisposition confirms prepared content and current parent state.
// It returns no applied outcome; callers cannot mistake preparation for an
// external effect. A stale parent or a substituted prepared tree is refused.
func VerifyChildDisposition(ctx context.Context, repository string, prepared PreparedChildDisposition) error {
	if err := verifyPreparedDisposition(ctx, repository, prepared); err != nil {
		return err
	}
	return CheckChildSnapshotCurrent(ctx, repository, prepared.ExpectedParent)
}

func verifyPreparedDisposition(ctx context.Context, repository string, prepared PreparedChildDisposition) error {
	if err := prepared.ExpectedParent.validate(); err != nil {
		return err
	}
	if prepared.Action != ChildMerge && prepared.Action != ChildReplace && prepared.Action != ChildDiscard {
		return fmt.Errorf("unknown child workspace disposition")
	}
	if prepared.Prepared.BaseSHA != prepared.ExpectedParent.Record.SnapshotSHA || prepared.Prepared.RepositoryKey != prepared.ExpectedParent.Record.RepositoryKey {
		return fmt.Errorf("prepared disposition parent mismatch")
	}
	if err := PinCommit(ctx, repository, prepared.Prepared); err != nil {
		return err
	}
	tree, err := snapshotObject(ctx, repository, prepared.Prepared.SnapshotSHA+"^{tree}")
	if err != nil {
		return err
	}
	if tree != prepared.TreeSHA {
		return fmt.Errorf("prepared disposition tree mismatch")
	}
	if err := verifySnapshotPolicy(ctx, repository, prepared.Prepared.SnapshotSHA, prepared.ExpectedParent.Policy); err != nil {
		return err
	}
	return nil
}
