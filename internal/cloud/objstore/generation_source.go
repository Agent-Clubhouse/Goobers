package objstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/goobers/goobers/internal/childworkflow"
	cloudlease "github.com/goobers/goobers/internal/cloud/lease"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runtimewiring"
)

// Source loads one instance's retained config generations.
type Source struct {
	metadata   *Metadata
	instanceID string
	tempRoot   string

	mu     sync.RWMutex
	handle *cloudlease.Handle

	// extract lays an archive out on disk; nil is archive.Extract. Tests
	// replace it to fail exactly at the extraction call site.
	extract func(context.Context, *configgeneration.Archive, string) error
}

// NewGenerationSource constructs an instance-scoped generation source.
func NewGenerationSource(
	metadata *Metadata,
	instanceID string,
	tempRoot string,
) (*Source, error) {
	if metadata == nil || instanceID == "" {
		return nil, errors.New("cloud generation source requires metadata and instance")
	}
	if tempRoot == "" {
		tempRoot = os.TempDir()
	}
	return &Source{
		metadata: metadata, instanceID: instanceID, tempRoot: tempRoot,
	}, nil
}

// BindFence installs the live controller epoch used by ephemeral read pins.
func (s *Source) BindFence(handle *cloudlease.Handle) {
	s.mu.Lock()
	s.handle = handle
	s.mu.Unlock()
}

// Acquire pins, verifies, extracts, loads, and read-protects one generation.
func (s *Source) Acquire(
	ctx context.Context,
	generation string,
) (childworkflow.Generation, error) {
	archiveData, err := s.metadata.GenerationArchive(
		ctx, s.instanceID, generation, configgeneration.MaxArchiveBytes)
	if err != nil {
		return childworkflow.Generation{}, err
	}
	archive, err := configgeneration.Decode(archiveData, generation)
	if err != nil {
		return childworkflow.Generation{}, fmt.Errorf(
			"%w: decode archive: %w", ErrGenerationInvalid, err)
	}
	if archive.InstanceID != s.instanceID {
		return childworkflow.Generation{}, fmt.Errorf(
			"%w: archive belongs to another instance",
			ErrGenerationInvalid)
	}
	s.mu.RLock()
	handle := s.handle
	s.mu.RUnlock()
	owner := "generation-reader:" + uuid.NewString()
	if err := s.metadata.PinGenerationReader(
		ctx, s.instanceID, generation, owner, handle); err != nil {
		return childworkflow.Generation{}, classifyPinError(err)
	}
	pinned := true
	var directory string
	cleanup := func() error {
		var errs []error
		if directory != "" {
			errs = append(errs, makeTreeWritable(directory))
			errs = append(errs, os.RemoveAll(directory))
		}
		if pinned {
			errs = append(errs, s.metadata.UnpinGeneration(
				context.WithoutCancel(ctx), s.instanceID, generation, owner))
			pinned = false
		}
		return errors.Join(errs...)
	}
	directory, err = os.MkdirTemp(s.tempRoot, "goobers-generation-")
	if err != nil {
		_ = cleanup()
		return childworkflow.Generation{}, materializeError("create generation directory", err)
	}
	extract := s.extract
	if extract == nil {
		extract = func(ctx context.Context, a *configgeneration.Archive, dir string) error { return a.Extract(ctx, dir) }
	}
	if err := extract(ctx, archive, directory); err != nil {
		_ = cleanup()
		return childworkflow.Generation{}, materializeError("extract archive", err)
	}
	configDir := filepath.Join(directory, "config")
	set, report, err := instance.LoadConfigDir(configDir)
	if err != nil {
		_ = cleanup()
		return childworkflow.Generation{}, fmt.Errorf(
			"%w: load retained config: %w (%s)",
			ErrGenerationInvalid, err,
			runtimewiring.ValidationIssueSummary(report))
	}
	if err := makeTreeReadOnly(directory); err != nil {
		_ = cleanup()
		return childworkflow.Generation{}, materializeError("protect generation directory", err)
	}
	var once sync.Once
	var releaseErr error
	return childworkflow.Generation{
		Directory: configDir,
		Set:       set,
		Release: func() error {
			once.Do(func() { releaseErr = cleanup() })
			return releaseErr
		},
	}, nil
}

// PinGeneration retains a generation for a durable child owner.
func (s *Source) PinGeneration(
	ctx context.Context,
	generation string,
	owner string,
) error {
	return s.metadata.PinGeneration(
		ctx, s.instanceID, generation, owner)
}

// UnpinGeneration releases a durable child owner's retention.
func (s *Source) UnpinGeneration(
	ctx context.Context,
	generation string,
	owner string,
) error {
	return s.metadata.UnpinGeneration(
		ctx, s.instanceID, generation, owner)
}

func classifyPinError(err error) error {
	// Pin follows a successful pointer read. A concurrent prune can remove the
	// pointer before the FK insert; the caller must classify that as missing,
	// not transient.
	var postgresError *pgconn.PgError
	if errors.As(err, &postgresError) &&
		postgresError.Code == "23503" {
		return fmt.Errorf("%w: generation pruned while pinning",
			ErrGenerationNotFound)
	}
	return err
}

func makeTreeReadOnly(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return os.Chmod(path, info.Mode().Perm()&^0222)
	})
}

func makeTreeWritable(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() | 0200
		if entry.IsDir() {
			mode |= 0700
		}
		return os.Chmod(path, mode)
	})
}

var _ childworkflow.GenerationSource = (*Source)(nil)

// materializeError classifies a failure to lay a generation out on local disk.
// Decode has already validated every entry and the digest, so these are local
// IO (a full or failing volume) or cancellation, never a bad archive: they are
// transient, not ErrGenerationInvalid.
func materializeError(step string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrGenerationUnavailable, step, err)
}
