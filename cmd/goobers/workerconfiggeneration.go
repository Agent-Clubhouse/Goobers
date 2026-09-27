package main

import (
	"context"
	"fmt"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
)

// Full execution archives use a private namespace on the fleet's shared
// volume. The stage-pod artifact HTTP plane does not front this namespace:
// pods receive only the worker-produced one-goober kit.
func (w *workerSeams) executionArchiveStore() (*blobstore.Dir, error) {
	root := instance.NewLayout(w.root).BlobStoreDir()
	if w.store != nil {
		directory, ok := w.store.(*blobstore.Dir)
		if !ok {
			return nil, fmt.Errorf("worker: config generations require the private fleet shared directory store")
		}
		root = directory.Root
	}
	return blobstore.NewDir(filepath.Join(root, "config-generations"))
}

func (w *workerSeams) snapshotForInvocation(ctx context.Context, env apiv1.InvocationEnvelope) (*workerConfigSnapshot, func(), error) {
	if env.ConfigGeneration == "" {
		snapshot, err := w.snapshotForPin(env.Gaggle, env.WorkflowID, env.GooberDigest)
		return snapshot, func() {}, err
	}
	if !instance.ValidIdentity(env.InstanceID) {
		return nil, nil, fmt.Errorf("worker: pinned config generation requires a valid instance identity")
	}
	store, err := w.executionArchiveStore()
	if err != nil {
		return nil, nil, err
	}
	data, err := store.GetBounded(ctx, env.ConfigGeneration, configgeneration.MaxArchiveBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("worker: load pinned config generation: %w", err)
	}
	archive, err := configgeneration.Decode(data, env.ConfigGeneration)
	if err != nil {
		return nil, nil, err
	}
	if archive.InstanceID != env.InstanceID {
		return nil, nil, fmt.Errorf("worker: config generation belongs to a different instance")
	}
	cache := configgeneration.Store{Root: filepath.Join(w.root, "worker-config-generations"), LocalCache: true}
	directory, lease, err := cache.KeepAndAcquire(ctx, data, env.ConfigGeneration, nil)
	if err != nil {
		return nil, nil, err
	}
	release := func() { _ = lease.Release() }
	snapshot, stable, err := w.loadConfigSnapshotAt(instance.NewLayout(w.root).WithConfigDir(directory))
	if err != nil {
		release()
		return nil, nil, err
	}
	if !stable {
		release()
		return nil, nil, fmt.Errorf("worker: immutable config generation changed while loading")
	}
	snapshot.generation = env.ConfigGeneration
	if env.GooberDigest != "" {
		actual, err := snapshot.gooberDigestFor(env.Gaggle, env.WorkflowID)
		if err != nil {
			release()
			return nil, nil, err
		}
		if actual != env.GooberDigest {
			release()
			return nil, nil, fmt.Errorf("worker: pinned config goober digest %s does not match run %s", actual, env.GooberDigest)
		}
	}
	return snapshot, release, nil
}

func (w *workerSeams) forInvocationGaggle(ctx context.Context, env apiv1.InvocationEnvelope, agentic bool) (*gaggleSeams, func(), error) {
	if env.ConfigGeneration == "" {
		var seams *gaggleSeams
		var err error
		if agentic {
			seams, err = w.forPinnedGaggle(env.Gaggle, env.WorkflowID, env.GooberDigest)
		} else {
			seams, err = w.forGaggle(env.Gaggle)
		}
		return seams, func() {}, err
	}
	snapshot, release, err := w.snapshotForInvocation(ctx, env)
	if err != nil {
		return nil, nil, err
	}
	built, err := w.buildGaggleSeams(snapshot, env.Gaggle)
	if err != nil {
		release()
		return nil, nil, err
	}
	return built.seams, release, nil
}
