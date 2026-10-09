package childpod

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/goobers/goobers/internal/recovery"
)

// GitState is parent-only custody for the committed and staged trees. Workspace
// remains the independent working tree. All three share one capture identity.
type GitState struct {
	Head  Carrier `json:"head"`
	Index Carrier `json:"index"`
}

func (s *GitState) snapshots() recovery.ParentGitState {
	return recovery.ParentGitState{Head: s.Head.Snapshot, Index: s.Index.Snapshot}
}

func validateGitState(parent bool, working *Carrier, state *GitState) error {
	if !parent || working == nil {
		if state != nil {
			return fmt.Errorf("parent Git state requires a workspace")
		}
		return nil
	}
	if state == nil {
		return fmt.Errorf("parent workspace requires independent Git state")
	}
	for _, c := range []Carrier{state.Head, state.Index} {
		if err := c.Validate(); err != nil {
			return err
		}
		a, b := c.Snapshot.Record, working.Snapshot.Record
		if a.RunID != b.RunID || a.RepositoryKey != b.RepositoryKey || !a.CreatedAt.Equal(b.CreatedAt) || !a.RetainUntil.Equal(b.RetainUntil) || !slices.Equal(c.Snapshot.Policy.ExcludedPaths, working.Snapshot.Policy.ExcludedPaths) {
			return fmt.Errorf("parent Git state differs from workspace source")
		}
	}
	return nil
}

func captureGitState(ctx context.Context, path string, snapshot recovery.ChildSnapshot) (*GitState, error) {
	state := &GitState{}
	for _, part := range []struct {
		staged bool
		out    *Carrier
	}{{false, &state.Head}, {true, &state.Index}} {
		var data bytes.Buffer
		portable, err := recovery.WritePortableGitState(ctx, path, snapshot, part.staged, &data, MaxBundleBytes)
		if err != nil {
			return nil, err
		}
		*part.out = Carrier{Snapshot: portable, Bundle: data.Bytes()}
	}
	return state, nil
}

// CaptureOutput is called only after the supervisor has quiesced all writers.
func CaptureOutput(ctx context.Context, path string, c Contract, digest string) (Output, error) {
	out := Output{Version: 1, ContractDigest: digest}
	if c.Workspace == nil {
		return out, nil
	}
	carrier, snapshot, err := CaptureCarrier(ctx, path, c.Workspace.Snapshot.Record.RepositoryKey, c.Identity.RunID, c.StartedAt, c.Workspace.Snapshot.Policy)
	if err != nil {
		return out, err
	}
	out.Workspace = &carrier
	if c.ParentOrigin != nil {
		out.GitState, err = captureGitState(ctx, path, snapshot)
	}
	return out, err
}

func importCarrier(ctx context.Context, path string, c Carrier) error {
	return withBundle(c, func(archive string) error {
		return recovery.ImportPortableSnapshot(ctx, path, archive, c.Snapshot, MaxBundleBytes)
	})
}

// MaterializeContract initializes a private worker checkout from the retained
// source. Parent commits, index, and files remain three distinct states.
func MaterializeContract(ctx context.Context, path string, c Contract) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Workspace == nil {
		return nil
	}
	if c.GitState == nil {
		return Materialize(ctx, path, *c.Workspace)
	}
	if err := Materialize(ctx, path, c.GitState.Head); err != nil {
		return err
	}
	for _, carrier := range []Carrier{c.GitState.Index, *c.Workspace} {
		if err := importCarrier(ctx, path, carrier); err != nil {
			return err
		}
	}
	return recovery.MaterializePortableGitState(ctx, path, c.GitState.Head.Snapshot, c.GitState.Index.Snapshot, c.Workspace.Snapshot)
}

func prepareOutputReturn(ctx context.Context, path string, expected recovery.ChildSnapshot, contract Contract, out Output, operation string) (recovery.ChildApplyPlan, error) {
	if contract.GitState == nil {
		return prepareReturn(ctx, path, expected, *out.Workspace, operation, contract.StartedAt)
	}
	for _, carrier := range []Carrier{*out.Workspace, out.GitState.Head, out.GitState.Index} {
		if err := importCarrier(ctx, path, carrier); err != nil {
			return recovery.ChildApplyPlan{}, err
		}
	}
	return recovery.PrepareParentApplication(ctx, path, expected, contract.GitState.snapshots(), out.GitState.snapshots(), out.Workspace.Snapshot, operation, contract.StartedAt, MaxBundleBytes)
}

func verifyRetainedGitState(plan recovery.ChildApplyPlan, contract Contract, out Output) error {
	if contract.GitState == nil {
		if plan.Parent != nil {
			return fmt.Errorf("child application cannot advance parent history")
		}
		return nil
	}
	if plan.Parent == nil || !reflect.DeepEqual(plan.Parent.Before, contract.GitState.snapshots()) || !reflect.DeepEqual(plan.Parent.After, out.GitState.snapshots()) {
		return fmt.Errorf("retained parent application differs from surrendered Git state")
	}
	return nil
}
