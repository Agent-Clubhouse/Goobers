package journal_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func fileBackendFactory(t *testing.T) backendFactory {
	t.Helper()
	return func() backendStore {
		root := t.TempDir()
		config := journal.FileBackendConfig{
			RunsDir: filepath.Join(root, "runs"), SchedulerDir: filepath.Join(root, "scheduler"),
		}
		return backendStore{Open: func() journal.Backend {
			return newCleanupBackend(t, journal.NewFileBackend(config))
		}}
	}
}

type cleanupBackend struct {
	journal.Backend
	t         *testing.T
	mu        sync.Mutex
	runs      map[journal.RunHandle]journal.OwnerToken
	instances map[journal.InstanceHandle]journal.OwnerToken
}

func newCleanupBackend(t *testing.T, backend journal.Backend) journal.Backend {
	t.Helper()
	tracked := &cleanupBackend{
		Backend: backend, t: t,
		runs: map[journal.RunHandle]journal.OwnerToken{}, instances: map[journal.InstanceHandle]journal.OwnerToken{},
	}
	t.Cleanup(tracked.cleanup)
	return tracked
}

func (b *cleanupBackend) CreateRun(ctx context.Context, request journal.CreateRunRequest) (journal.RunHandle, journal.OwnerToken, error) {
	handle, owner, err := b.Backend.CreateRun(ctx, request)
	if err == nil {
		b.trackRun(handle, owner)
	}
	return handle, owner, err
}

func (b *cleanupBackend) RecoverRun(ctx context.Context, key journal.RunKey, options journal.RunWriterOptions) (journal.RunHandle, journal.OwnerToken, journal.RecoverReport, error) {
	handle, owner, report, err := b.Backend.RecoverRun(ctx, key, options)
	if err == nil {
		b.trackRun(handle, owner)
	}
	return handle, owner, report, err
}

func (b *cleanupBackend) Retire(ctx context.Context, handle journal.RunHandle, owner journal.OwnerToken) error {
	if err := b.Backend.Retire(ctx, handle, owner); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.runs, handle)
	b.mu.Unlock()
	return nil
}

func (b *cleanupBackend) OpenInstance(ctx context.Context) (journal.InstanceHandle, journal.OwnerToken, journal.RecoverReport, error) {
	handle, owner, report, err := b.Backend.OpenInstance(ctx)
	if err == nil {
		b.trackInstance(handle, owner)
	}
	return handle, owner, report, err
}

func (b *cleanupBackend) BindInstance(appender journal.InstanceAppender) (journal.InstanceHandle, journal.OwnerToken, error) {
	handle, owner, err := b.Backend.BindInstance(appender)
	if err == nil {
		b.trackInstance(handle, owner)
	}
	return handle, owner, err
}

func (b *cleanupBackend) RetireInstance(ctx context.Context, handle journal.InstanceHandle, owner journal.OwnerToken) error {
	if err := b.Backend.RetireInstance(ctx, handle, owner); err != nil {
		return err
	}
	b.mu.Lock()
	delete(b.instances, handle)
	b.mu.Unlock()
	return nil
}

func (b *cleanupBackend) trackRun(handle journal.RunHandle, owner journal.OwnerToken) {
	b.mu.Lock()
	b.runs[handle] = owner
	b.mu.Unlock()
}

func (b *cleanupBackend) trackInstance(handle journal.InstanceHandle, owner journal.OwnerToken) {
	b.mu.Lock()
	b.instances[handle] = owner
	b.mu.Unlock()
}

func (b *cleanupBackend) cleanup() {
	b.mu.Lock()
	runs := b.runs
	instances := b.instances
	b.runs = map[journal.RunHandle]journal.OwnerToken{}
	b.instances = map[journal.InstanceHandle]journal.OwnerToken{}
	b.mu.Unlock()
	ctx := context.Background()
	for handle, owner := range runs {
		if err := b.Backend.Retire(ctx, handle, owner); err != nil {
			b.t.Errorf("retire leaked run %q: %v", handle, err)
		}
	}
	for handle, owner := range instances {
		if err := b.Backend.RetireInstance(ctx, handle, owner); err != nil {
			b.t.Errorf("retire leaked instance %q: %v", handle, err)
		}
	}
}

func TestJournalBackendConformance(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		if err := journalBackendConformance(fileBackendFactory(t)); err != nil {
			t.Fatal(err)
		}
	})
}
