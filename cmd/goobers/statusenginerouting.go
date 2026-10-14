package main

import (
	"context"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/workerrouting"
)

const (
	statusEngineRoutingTimeout = 5 * time.Second
	statusEngineRoutingRefresh = 30 * time.Second
)

// probeStatusEngineRouting is a seam so tests substitute a fake deployment.
var probeStatusEngineRouting = workerrouting.Probe

// statusEngineRouting reports whether the Temporal worker deployment routes
// tasks to the engine's live workers (#6020): a stalled cloud engine shows a
// Ready worker, zero poll errors and zero dispatch, so status says so. It is
// nil, and status unchanged, on an instance with no engine configured.
type statusEngineRouting struct {
	target   workerrouting.Target
	now      func() time.Time
	loadedAt time.Time
	value    workerrouting.Assessment
}

func newStatusEngineRouting(cfg *instance.Config) *statusEngineRouting {
	if !cfg.EngineProjectionEnabled() {
		return nil
	}
	engine := cfg.EffectiveEngineConfig()
	return &statusEngineRouting{now: time.Now, target: workerrouting.Target{
		HostPort: engine.HostPort, Namespace: engine.Namespace, TaskQueue: engine.TaskQueue, TLS: engine.TLS,
	}}
}

// Load probes at most once per refresh window, so --watch does not dial
// Temporal on every redraw.
func (s *statusEngineRouting) Load(ctx context.Context) *workerrouting.Assessment {
	if s == nil {
		return nil
	}
	if now := s.now(); s.loadedAt.IsZero() || !now.Before(s.loadedAt.Add(statusEngineRoutingRefresh)) {
		probeCtx, cancel := context.WithTimeout(ctx, statusEngineRoutingTimeout)
		s.value = probeStatusEngineRouting(probeCtx, s.target, now)
		cancel()
		s.loadedAt = now
	}
	value := s.value
	return &value
}

// engineRouting is built on first use, only for the full status board
// (needFleet): --agents and --runs-only stay network-free.
func (l *statusRunLoader) engineRouting() *statusEngineRouting {
	if l.routing == nil && l.needFleet && l.sources.Config != nil {
		l.routing = newStatusEngineRouting(l.sources.Config)
	}
	return l.routing
}

// collectionAndEngineText renders the partial-collection notice and the
// engine routing line for the status text board.
func (l *statusRunLoader) collectionAndEngineText(ctx context.Context) string {
	text := statusCollectionUnavailableText(l.collectionStatus())
	if routing := l.engineRouting().Load(ctx); routing != nil {
		text += routing.StatusLine()
	}
	return text
}

func (l *statusRunLoader) withEngineRouting(output statusJSONOutput) statusJSONOutput {
	output.EngineRouting = l.engineRouting().Load(context.Background())
	return output
}
