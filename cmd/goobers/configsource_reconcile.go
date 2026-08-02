package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/goobers/goobers/internal/instance"
)

type configSourceReconciler struct {
	source    instance.WorkflowSource
	reconcile func(context.Context, time.Time) error
	errors    *sweepErrorReporter
	wake      <-chan struct{}
}

func (r *configSourceReconciler) Run(ctx context.Context) error {
	watcher, watchEvents, watchErrors, err := watchGitRef(r.source)
	if err != nil {
		r.errors.report(fmt.Errorf("watch local workflow source: %w", err))
	} else if watcher != nil {
		defer watcher.Close()
	}

	ticker := time.NewTicker(configReloadInterval)
	defer ticker.Stop()
	reconcile := func(now time.Time) {
		err := r.reconcile(ctx, now)
		if err != nil && ctx.Err() != nil {
			return
		}
		r.errors.report(err)
	}
	reconcile(time.Now())

	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			reconcile(now)
		case <-r.wake:
			reconcile(time.Now())
		case _, ok := <-watchEvents:
			if !ok {
				watchEvents = nil
				continue
			}
			reconcile(time.Now())
		case watchErr, ok := <-watchErrors:
			if !ok {
				watchErrors = nil
				continue
			}
			r.errors.report(fmt.Errorf("watch local workflow source: %w", watchErr))
		}
	}
}

func watchGitRef(source instance.WorkflowSource) (*fsnotify.Watcher, <-chan fsnotify.Event, <-chan error, error) {
	if source.Kind != instance.WorkflowSourceKindGit || source.Path == "" {
		return nil, nil, nil, nil
	}
	gitDir, err := localGitDir(source.Path)
	if err != nil {
		return nil, nil, nil, err
	}
	ref := source.TrackedRef()
	ref = strings.TrimPrefix(ref, "refs/heads/")
	refDir := filepath.Dir(filepath.Join(gitDir, "refs", "heads", filepath.FromSlash(ref)))
	dirs := []string{gitDir, filepath.Join(gitDir, "refs"), filepath.Join(gitDir, "refs", "heads"), refDir}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, nil, nil, err
	}
	added := make(map[string]struct{}, len(dirs))
	for _, dir := range dirs {
		dir = filepath.Clean(dir)
		if _, ok := added[dir]; ok {
			continue
		}
		if info, statErr := os.Stat(dir); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			_ = watcher.Close()
			return nil, nil, nil, statErr
		} else if !info.IsDir() {
			continue
		}
		if err := watcher.Add(dir); err != nil {
			_ = watcher.Close()
			return nil, nil, nil, err
		}
		added[dir] = struct{}{}
	}
	return watcher, watcher.Events, watcher.Errors, nil
}

func localGitDir(repository string) (string, error) {
	repository, err := filepath.Abs(repository)
	if err != nil {
		return "", err
	}
	dotGit := filepath.Join(repository, ".git")
	info, err := os.Stat(dotGit)
	if err == nil && info.IsDir() {
		return dotGit, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if data, readErr := os.ReadFile(dotGit); readErr == nil {
		line := strings.TrimSpace(string(data))
		const prefix = "gitdir:"
		if !strings.HasPrefix(strings.ToLower(line), prefix) {
			return "", errors.New(".git file does not declare gitdir")
		}
		gitDir := strings.TrimSpace(line[len(prefix):])
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(repository, gitDir)
		}
		return filepath.Clean(gitDir), nil
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return "", readErr
	}
	if _, err := os.Stat(filepath.Join(repository, "HEAD")); err == nil {
		return repository, nil
	}
	return "", fmt.Errorf("%s is not a Git repository", repository)
}
