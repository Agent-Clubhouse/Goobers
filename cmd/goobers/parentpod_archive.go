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
	id, err := reader.Identity()
	if err != nil || id.Child != nil {
		return nil, err
	}
	ref, found, err := runner.ParentCleanupContribution(reader, target)
	if err != nil || !found {
		return nil, err
	}
	if err := childpod.VerifyParentCustody(ctx, reader); err != nil {
		return nil, err
	}
	data, err := reader.ArtifactBytesBounded(ref, childpod.MaxContractBytes)
	if err != nil {
		return nil, err
	}
	var preview childpod.Output
	if err := json.Unmarshal(data, &preview); err != nil {
		return nil, err
	}
	store := childpod.ParentBlobs{RunDir: reader.Dir(), Identity: id}
	input, err := store.Get(ctx, preview.ContractDigest)
	if err != nil {
		return nil, err
	}
	contract, err := childpod.DecodeContract(input, preview.ContractDigest)
	if err != nil {
		return nil, err
	}
	output, err := childpod.DecodeOutput(data, ref.Digest, preview.ContractDigest, contract)
	if err != nil {
		return nil, err
	}
	if contract.ParentOrigin == nil || !reflect.DeepEqual(contract.Identity, id) || output.Workspace == nil || output.Workspace.Snapshot.Record.RepositoryKey != repositoryKey {
		return nil, errors.New("parent cleanup source differs from held checkout")
	}
	policy := output.Workspace.Snapshot.Policy
	return &policy, nil
}
