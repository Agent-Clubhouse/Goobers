package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/instance"
	daemonservice "github.com/goobers/goobers/internal/service"
)

const (
	serviceSupervisorFailureStartup = "fatal-startup"
	serviceSupervisorFailureRuntime = "supervisor-exit"
	serviceReadinessLinePrefix      = "daemon started at "
)

type serviceSupervisorFailure struct {
	RecordedAt time.Time
	Kind       string
	Message    string
}

func (f serviceSupervisorFailure) statusPayload() *daemonservice.SupervisorFailure {
	if f.Kind == "" || f.Message == "" {
		return nil
	}
	return &daemonservice.SupervisorFailure{
		Kind:       f.Kind,
		Message:    f.Message,
		RecordedAt: f.RecordedAt.UTC().Format(time.RFC3339Nano),
	}
}

func appendServiceSupervisorFailure(layout instance.Layout, ready bool, err error) error {
	kind := serviceSupervisorFailureStartup
	if ready {
		kind = serviceSupervisorFailureRuntime
	}
	file, openErr := os.OpenFile(layout.DaemonLogFile(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if openErr != nil {
		return openErr
	}
	defer func() { _ = file.Close() }()
	_, writeErr := fmt.Fprintf(file, "%s %s: error: supervise daemon: %v\n", time.Now().UTC().Format(time.RFC3339Nano), kind, err)
	return writeErr
}

func latestServiceSupervisorFailure(path string) serviceSupervisorFailure {
	file, err := os.Open(path)
	if err != nil {
		return serviceSupervisorFailure{}
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return serviceSupervisorFailure{}
	}
	const maxTail = 64 << 10
	offset := max(info.Size()-maxTail, 0)
	buf := make([]byte, info.Size()-offset)
	if _, err := file.ReadAt(buf, offset); err != nil {
		return serviceSupervisorFailure{}
	}
	lines := bytes.Split(buf, []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		if failure, ok := parseServiceSupervisorFailureLine(strings.TrimSpace(string(lines[i]))); ok {
			return failure
		}
	}
	return serviceSupervisorFailure{}
}

func parseServiceSupervisorFailureLine(line string) (serviceSupervisorFailure, bool) {
	stamp, rest, ok := strings.Cut(line, " ")
	if !ok {
		return serviceSupervisorFailure{}, false
	}
	kind, message, ok := strings.Cut(rest, ":")
	if !ok || (kind != serviceSupervisorFailureStartup && kind != serviceSupervisorFailureRuntime) {
		return serviceSupervisorFailure{}, false
	}
	recordedAt, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return serviceSupervisorFailure{}, false
	}
	return serviceSupervisorFailure{
		RecordedAt: recordedAt.UTC(),
		Kind:       kind,
		Message:    strings.TrimSpace(message),
	}, true
}

type serviceReadinessTracker struct {
	writer io.Writer
	ready  atomic.Bool
	mu     sync.Mutex
	tail   string
}

func newServiceReadinessTracker(writer io.Writer) *serviceReadinessTracker {
	return &serviceReadinessTracker{writer: writer}
}

func (t *serviceReadinessTracker) Write(p []byte) (int, error) {
	t.observe(p)
	return t.writer.Write(p)
}

func (t *serviceReadinessTracker) Ready() bool {
	return t.ready.Load()
}

func (t *serviceReadinessTracker) observe(p []byte) {
	if t.ready.Load() {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	combined := t.tail + string(p)
	if strings.Contains(combined, serviceReadinessLinePrefix) {
		t.ready.Store(true)
	}
	const keep = len(serviceReadinessLinePrefix) - 1
	if len(combined) <= keep {
		t.tail = combined
		return
	}
	t.tail = combined[len(combined)-keep:]
}
