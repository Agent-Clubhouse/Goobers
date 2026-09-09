package configmirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/lock"
)

const seedMarker = ".config-mirror-seed"

// Seed initializes a private worker instance from one pinned mirror snapshot.
// Destination must be a dedicated child of a worker-owned volume, not its
// mount point. An init-container retry validates and retains a completed seed;
// it never overwrites an existing worker instance with a newer generation.
// Callers must validate the instance document and complete rendered config.
func Seed(ctx context.Context, mirror, destination string, validate func(string) error) (result error) {
	if !filepath.IsAbs(mirror) || !filepath.IsAbs(destination) || validate == nil {
		return errors.New("config mirror seed requires absolute paths and validation")
	}
	parent := filepath.Dir(destination)
	if parent == destination {
		return errors.New("config mirror seed destination must be a dedicated child directory")
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create seed parent: %w", err)
	}
	held, err := lock.TryAcquire(destination + ".seed.lock")
	if err != nil {
		return fmt.Errorf("lock seed destination: %w", err)
	}
	defer func() { _ = held.Release() }()
	if info, err := os.Lstat(destination); err == nil {
		if !info.IsDir() {
			return errors.New("worker seed destination is not a directory")
		}
		if err := checkSeedMarker(destination); err != nil {
			return fmt.Errorf("verify existing seed ownership: %w", err)
		}
		if err := validate(destination); err != nil {
			return fmt.Errorf("validate existing seed: %w", err)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	staging := destination + ".seed.pending"
	if err := reclaimValidation(staging); err != nil {
		return fmt.Errorf("reclaim previous seed staging: %w", err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		return fmt.Errorf("create seed staging: %w", err)
	}
	if err := writeValidationOwner(filepath.Join(staging, ".owner")); err != nil {
		return fmt.Errorf("mark seed staging ownership: %w", err)
	}
	defer func() {
		if err := reclaimValidation(staging); err != nil {
			result = errors.Join(result, fmt.Errorf("clean seed staging: %w", err))
		}
	}()
	if err := durability.SyncDir(staging); err != nil {
		return err
	}
	payload := filepath.Join(staging, "instance")
	if err := seedPayload(ctx, mirror, payload, validate); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := durability.Move(payload, destination); err != nil {
		return fmt.Errorf("publish validated seed directory: %w", err)
	}
	return durability.SyncDir(parent)
}

func seedPayload(ctx context.Context, mirror, destination string, validate func(string) error) error {
	snapshot, err := Open(mirror)
	if err != nil {
		return fmt.Errorf("open seed mirror snapshot: %w", err)
	}
	defer func() { _ = snapshot.Close() }()
	if err := os.Mkdir(destination, 0o700); err != nil {
		return fmt.Errorf("create seed payload: %w", err)
	}
	if err := snapshot.Extract(ctx, destination); err != nil {
		return fmt.Errorf("extract seed snapshot: %w", err)
	}
	if err := validate(destination); err != nil {
		return fmt.Errorf("validate extracted seed: %w", err)
	}
	if err := writeValidationOwner(filepath.Join(destination, seedMarker)); err != nil {
		return fmt.Errorf("mark completed seed ownership: %w", err)
	}
	return durability.SyncDir(destination)
}

func checkSeedMarker(destination string) error {
	marker := filepath.Join(destination, seedMarker)
	info, err := os.Lstat(marker)
	if err != nil {
		return errors.New("refusing to overwrite an instance not initialized by config mirror seed")
	}
	if !info.Mode().IsRegular() || info.Size() != int64(len(validationOwner)) {
		return errors.New("invalid worker config seed marker")
	}
	f, err := os.Open(marker)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, int64(len(validationOwner)+1)))
	if err != nil {
		return err
	}
	if string(data) != validationOwner {
		return errors.New("invalid worker config seed ownership")
	}
	return nil
}
