package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/parallelworkspace"
	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/runner"
)

type parentArchiveAuthority struct {
	identity      journal.RunIdentity
	branch        int
	repositoryKey string
	policy        recovery.SnapshotPolicy
}

// Resolve typed source authority before archive creation, restoration or cleanup.
// A fork is authorized by the host plan; a returned pod still requires its exact
// verified worker contract and independently acknowledged writer custody.
func parentArchiveSource(ctx context.Context, reader *journal.Reader, archive runner.ParentWorkspaceArchive) (parentArchiveAuthority, error) {
	var authority parentArchiveAuthority
	if archive.Fork != nil {
		plan, branch, err := runner.ParentForkArchivePlan(reader, archive)
		if err != nil {
			return authority, err
		}
		source, err := parallelworkspace.ReadSource(reader, plan.Source, plan.Parallel, plan.Sequence)
		if err != nil {
			return authority, err
		}
		id, err := reader.Identity()
		if err != nil {
			return authority, err
		}
		return parentArchiveAuthority{identity: id, branch: branch, repositoryKey: source.Record.RepositoryKey, policy: source.Policy}, nil
	}
	data, err := reader.ArtifactBytesBounded(archive.Output, childpod.MaxContractBytes)
	if err != nil {
		return authority, err
	}
	var preview childpod.Output
	if json.Unmarshal(data, &preview) != nil || preview.Workspace == nil || preview.ContractDigest != archive.ContractDigest {
		return authority, errors.New("parent archive output differs from contribution")
	}
	key := preview.Workspace.Snapshot.Record.RepositoryKey
	contract, output, err := readParentArchiveOutput(ctx, reader, archive.Output, key)
	if err != nil {
		return authority, err
	}
	if !reflect.DeepEqual(contract.ParentOrigin, archive.Custody.Origin) || contract.Workspace == nil {
		return authority, errors.New("parent archive origin differs from retained worker")
	}
	return parentArchiveAuthority{identity: contract.Identity, branch: contract.ParentBranch, repositoryKey: key, policy: output.Workspace.Snapshot.Policy}, nil
}
