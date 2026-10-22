package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/configmirror"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/retryutil"
	"github.com/goobers/goobers/internal/workerblob"
)

type workerConfigPlaneSetup struct {
	client configmirror.Client
	root   string
	digest string
}

type workerConfigPlaneUsageError struct{ error }

func prepareWorkerConfigPlane(endpoint, keyFile, workRoot string) (workerConfigPlaneSetup, error) {
	if endpoint == "" {
		return workerConfigPlaneSetup{}, nil
	}
	if err := workerblob.ValidateEndpoint(endpoint); err != nil {
		return workerConfigPlaneSetup{}, workerConfigPlaneUsageError{fmt.Errorf("--config-endpoint: %w", err)}
	}
	tokenSource, err := workerConfigPlaneTokenSource(keyFile)
	if err != nil {
		return workerConfigPlaneSetup{}, workerConfigPlaneUsageError{err}
	}
	client := configmirror.Client{BaseURL: endpoint, TokenSource: tokenSource}
	root, digest, err := fetchInitialWorkerConfig(context.Background(), client, filepath.Join(workRoot, "config-snapshots"))
	if err != nil {
		return workerConfigPlaneSetup{}, fmt.Errorf("fetch worker config: %w", err)
	}
	return workerConfigPlaneSetup{client: client, root: root, digest: digest}, nil
}

func prepareWorkerConfigPlaneCLI(endpoint, keyFile, workRoot string, instanceRoot *string, stderr io.Writer) (workerConfigPlaneSetup, int) {
	if endpoint != "" && *instanceRoot != "" {
		_, _ = fmt.Fprintln(stderr, "error: --config-endpoint cannot be combined with --instance or GOOBERS_INSTANCE_ROOT")
		return workerConfigPlaneSetup{}, 2
	}
	setup, err := prepareWorkerConfigPlane(endpoint, keyFile, workRoot)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		var usage workerConfigPlaneUsageError
		if errors.As(err, &usage) {
			return workerConfigPlaneSetup{}, 2
		}
		return workerConfigPlaneSetup{}, 1
	}
	if setup.root != "" {
		*instanceRoot = setup.root
	}
	return setup, 0
}

func resolvedWorkerRoot(configured string) string {
	if configured != "" {
		return configured
	}
	return defaultWorkerRoot(os.TempDir())
}

type workerConfigPlaneWatcher struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func workerConfigPlaneTokenSource(keyFile string) (func() (string, error), error) {
	if keyFile == "" {
		return nil, fmt.Errorf("worker config plane requires --config-key-file")
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("read worker config key: %w", err)
	}
	signer, err := podauth.NewSignedKey(bytes.TrimSpace(key))
	if err != nil {
		return nil, err
	}
	workerID := workerDivergenceWorkerID()
	return func() (string, error) {
		return signer.MintWorkerConfigDigest(workerID, 2*time.Minute)
	}, nil
}

func fetchInitialWorkerConfig(ctx context.Context, client configmirror.Client, cacheRoot string) (string, string, error) {
	var root, digest string
	err := retryutil.Until(ctx, workerblob.DefaultReadyWait, retryutil.Policy{Base: 500 * time.Millisecond, Max: 15 * time.Second}, func(ctx context.Context) (bool, error) {
		var err error
		digest, err = client.Current(ctx)
		if err != nil {
			return configmirror.Retryable(err), err
		}
		root, err = client.Materialize(ctx, cacheRoot, digest, validateSeededWorkerConfig)
		return configmirror.Retryable(err), err
	})
	return root, digest, err
}

func startWorkerConfigPlaneWatcher(parent context.Context, seams *workerSeams, client configmirror.Client, cacheRoot, current string, interval time.Duration) *workerConfigPlaneWatcher {
	ctx, cancel := context.WithCancel(parent)
	watcher := &workerConfigPlaneWatcher{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(watcher.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			digest, err := client.Current(ctx)
			if err != nil {
				seams.log("worker config plane: current digest unavailable: %v", err)
				continue
			}
			if digest == current {
				continue
			}
			root, err := client.Materialize(ctx, cacheRoot, digest, validateSeededWorkerConfig)
			if err != nil {
				seams.log("worker config plane: fetch %s: %v", digest, err)
				continue
			}
			outcome, err := seams.reloadFromLayout(instance.NewLayout(root))
			if err != nil {
				seams.log("worker config plane: reject %s: %v", digest, err)
				continue
			}
			current = digest
			seams.log("worker config plane: applied %s (tree %s); retaining %d superseded tree(s)", digest, outcome.Digest, len(outcome.RetainedDigests))
			if err := pruneWorkerConfigCache(cacheRoot, seams.retainedConfigRoots()); err != nil {
				seams.log("worker config plane: prune cache: %v", err)
			}
		}
	}()
	return watcher
}

func (w *workerConfigPlaneWatcher) Stop() {
	if w == nil {
		return
	}
	w.cancel()
	<-w.done
}

type workerConfigUpdateWatcher interface{ Stop() }

func configureWorkerConfigSeams(seams *workerSeams, historyDepth int, plane workerConfigPlaneSetup) {
	seams.historyDepth = historyDepth
	if plane.root != "" {
		seams.configPlane = &plane.client
	}
}

func startWorkerConfigUpdates(seams *workerSeams, plane workerConfigPlaneSetup, workRoot string, interval time.Duration) (workerConfigUpdateWatcher, string) {
	if interval <= 0 {
		return nil, "goobers worker: config reload disabled; seams stay pinned to the boot-time config tree"
	}
	if plane.root != "" {
		watcher := startWorkerConfigPlaneWatcher(context.Background(), seams, plane.client, filepath.Join(workRoot, "config-snapshots"), plane.digest, interval)
		return watcher, fmt.Sprintf("goobers worker: config plane refresh every %s; retaining %d superseded config tree(s)", interval, seams.historyDepth)
	}
	watcher := startWorkerConfigWatcher(context.Background(), seams, interval)
	return watcher, fmt.Sprintf("goobers worker: config reload every %s from %s; retaining %d superseded config tree(s) for in-flight goober-digest pins",
		interval, seams.root, seams.historyDepth)
}

func (w *workerSeams) retainedConfigRoots() map[string]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	roots := make(map[string]bool, len(w.history)+1)
	if current := w.snapshot.Load(); current != nil {
		roots[filepath.Dir(current.configDir)] = true
	}
	for _, snapshot := range w.history {
		roots[filepath.Dir(snapshot.configDir)] = true
	}
	return roots
}

func pruneWorkerConfigCache(cacheRoot string, keep map[string]bool) error {
	entries, err := os.ReadDir(cacheRoot)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(cacheRoot, entry.Name())
		if !entry.IsDir() || keep[path] {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}
