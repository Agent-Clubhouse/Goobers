package main

import (
	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/telemetry"
)

func selfExecutionObserver(cfg *instance.Config, tel *telemetry.Client) func(bool) {
	cfg.StartSelfExecutionAccounting()
	tel.SelfExecutionPolicy(cfg.SelfExecutionDenied())
	return func(refused bool) { cfg.ObserveSelfExecution(refused); tel.SelfExecutionObserved(refused) }
}

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
	if snapshot.cfg == nil {
		return engine.ErrNotConfigured
	}
	cfg := snapshot.cfg
	denied := cfg.SelfExecutionDenied()
	cfg.ObserveSelfExecution(denied)
	if denied {
		return &runsemantics.SelfExecutionRefusal{Stage: stage}
	}
	return nil
}

func withSelfExecutionPolicy(rc runner.Config, cfg *instance.Config, tel *telemetry.Client) runner.Config {
	rc.TrustedLocalParents = trustedLocalChildren(cfg)
	rc.SelfExecutionDenied = cfg.SelfExecutionDenied()
	rc.SelfExecutionObserved = selfExecutionObserver(cfg, tel)
	return rc
}
