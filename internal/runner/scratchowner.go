package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mutationsidecar"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/safeopen"
)

// The owner is outside the directory exposed to the stage. The whole host
// container is reaped together, so owner records cannot accumulate separately.
const scratchOwnerFile = "owner-run-id"
const scratchPayloadDir = "workspace"

func createOwnedScratch(root, runsDir, owner string) (*stageWorkspace, error) {
	if !apiv1.ValidRunID(owner) {
		return nil, fmt.Errorf("scratch workspace requires a valid owning run")
	}
	container, err := os.MkdirTemp(root, scratchWorkspacePrefix+"*")
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(container, scratchOwnerFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err == nil {
		_, err = file.WriteString(owner)
		if err == nil {
			err = file.Sync()
		}
		err = errors.Join(err, file.Close())
	}
	path := filepath.Join(container, scratchPayloadDir)
	if err == nil {
		err = os.Mkdir(path, 0700)
	}
	if err == nil {
		err = durability.SyncDir(container)
	}
	if err == nil {
		err = durability.SyncDir(root)
	}
	if err != nil {
		// This container has never been handed to a stage.
		return nil, errors.Join(err, os.RemoveAll(container))
	}
	return &stageWorkspace{path: path, scratchContainer: container, scratchRunsDir: runsDir}, nil
}

func removeOwnedScratch(ctx context.Context, container, runsDir string) error {
	if runsDir == "" {
		return fmt.Errorf("scratch cleanup requires a host-selected run root")
	}
	data, err := readScratchOwner(container)
	if err != nil {
		return fmt.Errorf("read scratch workspace owner: %w", err)
	}
	owner := string(data)
	if !apiv1.ValidRunID(owner) {
		return fmt.Errorf("invalid scratch workspace owner; preserving workspace")
	}
	path := filepath.Join(container, scratchPayloadDir)
	if err := mutationsidecar.RecoverBeforeCleanup(ctx, path, filepath.Base(container), owner, filepath.Join(runsDir, owner)); err != nil {
		return err
	}
	return os.RemoveAll(container)
}

func readScratchOwner(container string) ([]byte, error) {
	dir, err := safeopen.Open(container)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	file, err := safeopen.OpenAt(dir, scratchOwnerFile)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 256 {
		return nil, fmt.Errorf("invalid scratch owner record")
	}
	data, err := io.ReadAll(io.LimitReader(file, 257))
	if len(data) > 256 {
		return nil, fmt.Errorf("scratch owner record exceeds limit")
	}
	return data, err
}
