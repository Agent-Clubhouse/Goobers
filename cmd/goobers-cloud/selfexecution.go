package main

import (
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/runsemantics"
)

// admitSelfExecution shares first-use snapshot initialization with the executor
// seams. Admission runs before those seams and must work without a reload
// watcher (including --config-reload-interval 0).
func (w *workerSeams) admitSelfExecution(stage string) error {
	snapshot := w.snapshot.Load()
	if snapshot == nil {
		if w.root == "" {
			return engine.ErrNotConfigured
		}
		w.mu.Lock()
		var err error
		snapshot, err = w.currentSnapshotLocked()
		w.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if snapshot.Config == nil {
		return engine.ErrNotConfigured
	}
	cfg := snapshot.Config
	denied := cfg.SelfExecutionDenied()
	cfg.ObserveSelfExecution(denied)
	if denied {
		return &runsemantics.SelfExecutionRefusal{Stage: stage}
	}
	return nil
}
