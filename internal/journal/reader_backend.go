package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// RunRef identifies one logical run.
type RunRef struct {
	ID       string
	ReadOnly bool
}

// RunFilter selects a bounded lexical page.
type RunFilter struct {
	After string
	Limit int
}

// RunReader is the typed read surface used by shared control-plane readers.
// A handle is bound to the OpenRun context. Callers must keep that context alive
// for the entire read; backends must honor cancellation during subsequent I/O.
type RunReader interface {
	Schema() SchemaInfo
	Identity() (RunIdentity, error)
	State() (State, error)
	Phase() (RunPhase, error)
	PhaseBounded(context.Context) (RunPhase, error)
	Events() ([]Event, error)
	EventRecords() ([]EventRecord, error)
	ArtifactBytes(Ref) ([]byte, error)
	ArtifactBytesBounded(Ref, int64) ([]byte, error)
	ArtifactByDigest(string) ([]byte, error)
	SpanBytes(Ref) ([]byte, error)
}

// ReaderBackend is the shared journal read contract.
type ReaderBackend interface {
	OpenRun(context.Context, RunRef) (RunReader, error)
	ReadEvents(context.Context, RunRef, uint64, int) ([]EventRecord, error)
	ListRuns(context.Context, RunFilter) ([]RunRef, error)
	InstanceRead(context.Context, uint64, int) ([]Event, error)
}

// StallActivity is one running journal's durable activity-clock observation.
// Database-backed implementations populate DatabaseNow from the same database
// statement that reads LastAppendAt.
type StallActivity struct {
	Identity     RunIdentity
	LastAppendAt time.Time
	DatabaseNow  time.Time
}

// StallBackend is the bounded activity scan used by shared stall scheduling.
type StallBackend interface {
	StallCandidates(context.Context, string, int) ([]StallActivity, error)
}

// FileReaderBackendConfig locates local journals without exposing paths to callers.
type FileReaderBackendConfig struct {
	RunsDirs     []string
	ResolveRun   func(context.Context, string) (string, error)
	ResolveRoots func(context.Context) ([]string, error)
	SchedulerDir string
}

// FileReaderBackend reads today's run-directory and instance-log layout.
type FileReaderBackend struct{ config FileReaderBackendConfig }

// NewFileReaderBackend returns the local file reader.
func NewFileReaderBackend(config ...FileReaderBackendConfig) ReaderBackend {
	var cfg FileReaderBackendConfig
	if len(config) > 0 {
		cfg = config[0]
	}
	return FileReaderBackend{config: cfg}
}

// OpenRun opens one run, optionally refusing schema migration.
func (b FileReaderBackend) OpenRun(ctx context.Context, ref RunRef) (RunReader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := b.resolveRun(ctx, ref.ID)
	if err != nil {
		return nil, err
	}
	var reader *Reader
	if ref.ReadOnly {
		reader, err = OpenReadOnly(dir)
	} else {
		reader, err = OpenRead(dir)
	}
	if err != nil {
		return nil, err
	}
	return contextRunReader{RunReader: reader, ctx: ctx}, nil
}

// ReadEvents reads a bounded sequence page after afterSeq.
func (b FileReaderBackend) ReadEvents(ctx context.Context, ref RunRef, afterSeq uint64, limit int) ([]EventRecord, error) {
	reader, err := b.OpenRun(ctx, ref)
	if err != nil {
		return nil, err
	}
	records, err := reader.EventRecords()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = len(records)
	}
	result := make([]EventRecord, 0, min(limit, len(records)))
	for _, record := range records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if record.Event.Seq > afterSeq {
			result = append(result, record)
		}
		if len(result) == limit {
			break
		}
	}
	return result, nil
}

// ListRuns lists valid run identities in lexical order.
func (b FileReaderBackend) ListRuns(ctx context.Context, filter RunFilter) ([]RunRef, error) {
	roots, err := b.roots(ctx)
	if err != nil {
		return nil, err
	}
	var refs []RunRef
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if entry.IsDir() && apiv1.ValidRunID(entry.Name()) && entry.Name() > filter.After {
				refs = append(refs, RunRef{ID: entry.Name()})
			}
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	if filter.Limit > 0 && len(refs) > filter.Limit {
		refs = refs[:filter.Limit]
	}
	return refs, nil
}

// InstanceRead reads instance events after afterSeq with an optional limit.
func (b FileReaderBackend) InstanceRead(ctx context.Context, afterSeq uint64, limit int) ([]Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.config.SchedulerDir == "" {
		return nil, errors.New("journal: instance journal location is unavailable")
	}
	events, err := ReadInstanceLogAfterSeq(b.config.SchedulerDir, afterSeq)
	if err != nil {
		return nil, err
	}
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	return events, nil
}

func (b FileReaderBackend) resolveRun(ctx context.Context, runID string) (string, error) {
	if !apiv1.ValidRunID(runID) {
		return "", ErrNotRunDirectory
	}
	if b.config.ResolveRun != nil {
		return b.config.ResolveRun(ctx, runID)
	}
	roots, err := b.roots(ctx)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		dir := filepath.Join(root, runID)
		if info, err := os.Lstat(dir); err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			// Only an absent identity is unpublished. Malformed or unreadable
			// identities must reach admission instead of being hidden.
			if _, err := os.Stat(filepath.Join(dir, fileRunYAML)); err == nil {
				return dir, nil
			} else if !errors.Is(err, os.ErrNotExist) {
				return "", err
			}
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return "", os.ErrNotExist
}

func (b FileReaderBackend) roots(ctx context.Context) ([]string, error) {
	if b.config.ResolveRoots != nil {
		return b.config.ResolveRoots(ctx)
	}
	return append([]string(nil), b.config.RunsDirs...), nil
}

var (
	_ RunReader     = (*Reader)(nil)
	_ ReaderBackend = FileReaderBackend{}
)
