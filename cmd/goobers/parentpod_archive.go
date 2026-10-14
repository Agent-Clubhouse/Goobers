package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/worktree"
)

func parentCleanupPolicy(ctx context.Context, reader *journal.Reader, target worktree.CleanupTarget, repositoryKey string) (*recovery.SnapshotPolicy, error) {
	archive, found, err := runner.ParentCleanupWorkspace(reader, target)
	if err != nil || !found {
		return nil, err
	}
	authority, err := parentArchiveSource(ctx, reader, archive)
	if err != nil {
		return nil, err
	}
	if authority.repositoryKey != repositoryKey {
		return nil, errors.New("parent cleanup source repository changed")
	}
	return &authority.policy, nil
}

func readParentArchiveOutput(ctx context.Context, reader *journal.Reader, ref journal.Ref, repositoryKey string) (childpod.Contract, childpod.Output, error) {
	var empty childpod.Contract
	var emptyOutput childpod.Output
	id, err := reader.Identity()
	if err != nil {
		return empty, emptyOutput, err
	}
	if err := childpod.VerifyParentCustody(ctx, reader); err != nil {
		return empty, emptyOutput, err
	}
	data, err := reader.ArtifactBytesBounded(ref, childpod.MaxContractBytes)
	if err != nil {
		return empty, emptyOutput, err
	}
	var preview childpod.Output
	if err := json.Unmarshal(data, &preview); err != nil {
		return empty, emptyOutput, err
	}
	store := childpod.ParentBlobs{RunDir: reader.Dir(), Identity: id}
	input, err := store.Get(ctx, preview.ContractDigest)
	if err != nil {
		return empty, emptyOutput, err
	}
	contract, err := childpod.DecodeContract(input, preview.ContractDigest)
	if err != nil {
		return empty, emptyOutput, err
	}
	output, err := childpod.DecodeOutput(data, ref.Digest, preview.ContractDigest, contract)
	if err != nil {
		return empty, emptyOutput, err
	}
	if contract.ParentOrigin == nil || !reflect.DeepEqual(contract.Identity, id) || output.Workspace == nil || output.Workspace.Snapshot.Record.RepositoryKey != repositoryKey {
		return empty, emptyOutput, errors.New("parent cleanup source differs from held checkout")
	}
	return contract, output, nil
}
