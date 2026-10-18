package recovery

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"
)

// ParentGitState supplements a working-tree carrier with independent committed
// and staged trees. Synthetic transport ancestry is never published.
type ParentGitState struct {
	Head  PortableSnapshot `json:"head"`
	Index PortableSnapshot `json:"index"`
}

// ParentApplication is host-authored durable intent. A worker's committed tree
// becomes one checkpoint on the original host ancestry; its staged and working
// trees remain independent. The exact intent is reused after interrupted apply.
type ParentApplication struct {
	Before  ParentGitState `json:"before"`
	After   ParentGitState `json:"after"`
	HeadSHA string         `json:"headSha"`
}

// PrepareParentApplication requires imported, verified portable objects and
// exclusive host custody. It changes no branch, index, or working files.
func PrepareParentApplication(ctx context.Context, repository string, expected ChildSnapshot, before, after ParentGitState, working PortableSnapshot, operation string, at time.Time, maxBytes int64) (ChildApplyPlan, error) {
	plan, err := PreparePortableReturn(ctx, repository, expected, working, operation, at, maxBytes)
	if err != nil {
		return plan, err
	}
	plan.Parent = &ParentApplication{Before: before, After: after}
	if err := verifyParentStates(ctx, repository, plan); err != nil {
		return ChildApplyPlan{}, err
	}
	for _, part := range []struct {
		staged bool
		state  PortableSnapshot
	}{{false, before.Head}, {true, before.Index}} {
		captured, err := WritePortableGitState(ctx, repository, expected, part.staged, io.Discard, maxBytes)
		if err != nil {
			return ChildApplyPlan{}, err
		}
		if captured.TreeSHA != part.state.TreeSHA {
			return ChildApplyPlan{}, ErrWorkspaceChanged
		}
	}
	plan.Parent.HeadSHA, err = mappedParentHead(ctx, repository, plan)
	if err != nil {
		return ChildApplyPlan{}, err
	}
	changes, err := parentIndexChanges(ctx, repository, plan)
	if err != nil {
		return ChildApplyPlan{}, err
	}
	plan.AppliedIndexDigest, err = planChildIndex(ctx, repository, expected.Record.BaseSHA, changes)
	return plan, err
}

func verifyParentStates(ctx context.Context, repository string, plan ChildApplyPlan) error {
	e := plan.Disposition.ExpectedParent
	p := plan.Parent
	if p == nil || plan.Disposition.Action != ChildReplace {
		return fmt.Errorf("invalid parent Git-state application")
	}
	for _, state := range []PortableSnapshot{p.Before.Head, p.Before.Index, p.After.Head, p.After.Index} {
		if err := state.Validate(); err != nil {
			return err
		}
		if state.Record.RepositoryKey != e.Record.RepositoryKey || state.Record.RunID != e.Record.RunID || !state.Record.CreatedAt.Equal(e.Record.CreatedAt) || !slices.Equal(state.Policy.ExcludedPaths, e.Policy.ExcludedPaths) {
			return fmt.Errorf("parent Git-state source mismatch")
		}
		if err := verifyPortableObjects(ctx, repository, state); err != nil {
			return err
		}
	}
	return nil
}

func parentIndexChanges(ctx context.Context, repository string, plan ChildApplyPlan) ([]childFileChange, error) {
	p := plan.Disposition
	p.ExpectedParent.TreeSHA = plan.Parent.Before.Index.TreeSHA
	p.TreeSHA = plan.Parent.After.Index.TreeSHA
	return loadChildChanges(ctx, repository, p)
}

func mappedParentHead(ctx context.Context, repository string, plan ChildApplyPlan) (string, error) {
	p := plan.Parent
	e := plan.Disposition.ExpectedParent
	if p.Before.Head.TreeSHA == p.After.Head.TreeSHA {
		return e.Record.BaseSHA, nil
	}
	directory, err := privateGitDirectory(ctx, repository, "goobers-parent-commit-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	env, err := snapshotEnvironment(ctx, repository, directory, e.Record.BaseSHA)
	if err != nil {
		return "", err
	}
	if err := recoveryGitWithEnv(ctx, repository, io.Discard, env, "read-tree", p.After.Head.TreeSHA); err != nil {
		return "", err
	}
	if err := restorePublicationOmissions(ctx, repository, env, e); err != nil {
		return "", err
	}
	tree, err := privateIndexTree(ctx, repository, env)
	if err != nil {
		return "", err
	}
	date := plan.Disposition.Prepared.CreatedAt.UTC().Format(time.RFC3339)
	env = append(env, "GIT_AUTHOR_NAME=Goobers", "GIT_AUTHOR_EMAIL=goobers@goobers.invalid", "GIT_COMMITTER_NAME=Goobers", "GIT_COMMITTER_EMAIL=goobers@goobers.invalid", "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	var output boundedRefOutput
	message := "Checkpoint isolated parent stage\n\nApplication: " + plan.Disposition.Prepared.SnapshotSHA
	if err := recoveryGitWithEnv(ctx, repository, &output, env, "-c", "commit.gpgsign=false", "commit-tree", tree, "-p", e.Record.BaseSHA, "-m", message); err != nil {
		return "", err
	}
	head := strings.TrimSpace(output.String())
	if !gitObjectID.MatchString(head) {
		return "", fmt.Errorf("invalid parent checkpoint commit")
	}
	return head, nil
}

func applicationHead(ctx context.Context, repository string, plan ChildApplyPlan) (string, error) {
	if plan.Parent == nil {
		return plan.Disposition.ExpectedParent.Record.BaseSHA, nil
	}
	if err := verifyParentStates(ctx, repository, plan); err != nil {
		return "", err
	}
	head, err := mappedParentHead(ctx, repository, plan)
	if err != nil {
		return "", err
	}
	if head != plan.Parent.HeadSHA {
		return "", fmt.Errorf("parent checkpoint differs from durable intent")
	}
	return head, nil
}

func applyApplicationIndex(ctx context.Context, repository string, plan ChildApplyPlan, changes []childFileChange) error {
	if plan.Parent != nil {
		var err error
		changes, err = parentIndexChanges(ctx, repository, plan)
		if err != nil {
			return err
		}
	}
	return updateChildIndex(ctx, repository, nil, changes)
}
