package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goobers/goobers/providers"
)

// ProviderRateLimitEventName is the stable span event name for provider backoff.
const ProviderRateLimitEventName = "provider.rate_limit"

// StageRateLimitObserver writes provider rate-limit decisions to the current
// stage's telemetry sidecar using only credential-safe dimensions.
type StageRateLimitObserver struct {
	dir string
	now func() time.Time
	mu  sync.Mutex
}

// NewStageRateLimitObserver adapts provider rate-limit decisions into stage
// telemetry events. An empty directory makes the observer a no-op.
func NewStageRateLimitObserver(dir string) *StageRateLimitObserver {
	return &StageRateLimitObserver{dir: dir, now: time.Now}
}

// ObserveRateLimit implements providers.RateLimitObserver.
func (o *StageRateLimitObserver) ObserveRateLimit(_ context.Context, ev providers.RateLimitEvent) {
	if o == nil || o.dir == "" {
		return
	}
	event := stageEvent{
		Time: o.now(),
		Name: ProviderRateLimitEventName,
		Attrs: map[string]any{
			"provider": ev.Provider,
			"scope":    ev.Scope,
			"delay_ms": ev.Delay.Milliseconds(),
			"outcome":  ev.Outcome,
		},
	}
	if !validEvent(event) || !isPlainDir(o.dir) {
		return
	}
	data, err := json.Marshal(event)
	if err != nil {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	path := filepath.Join(o.dir, eventsFile)
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return
	} else if err != nil && !os.IsNotExist(err) {
		return
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	_, _ = file.Write(append(data, '\n'))
}
