package journal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goobers/goobers/internal/panicguard"
)

// CommittedEvent owns the scrubbed bytes of one successful event-file commit.
// Body excludes the JSONL newline; it must not be reconstructed from Event,
// whose in-memory fields have not passed through the boundary scrubber.
type CommittedEvent struct {
	Kind, JournalID, InstanceID, Gaggle, RunID string
	Seq                                        uint64
	Time, ObservedTime                         time.Time
	Body                                       []byte
}

// CommittedEventSink accepts ownership of an immutable event. Commit MUST only
// offer it to a bounded, nonblocking queue and count rejected offers. It runs
// under the journal's write lock: no I/O, logging, or exporter callbacks belong
// here. Export and error reporting belong to the queue's worker.
type CommittedEventSink interface {
	Commit(CommittedEvent)
}

type commitRegistration struct {
	mu         sync.RWMutex
	sink       CommittedEventSink
	instanceID string
	active     atomic.Bool
}

var committedSinks = struct {
	sync.RWMutex
	roots map[string]*commitRegistration
	count atomic.Int64
}{roots: make(map[string]*commitRegistration)}

// RegisterCommittedEventSink attaches a process-local sink to one existing
// instance root. Overlapping registrations are rejected, not replaced. The
// returned idempotent unregister stops new offers (including from open handles)
// before returning; the owner can then drain and shut down its sink.
func RegisterCommittedEventSink(root, instanceID string, sink CommittedEventSink) (func(), error) {
	if sink == nil {
		return nil, errors.New("journal: committed event sink is nil")
	}
	root, err := canonicalJournalPath(root)
	if err != nil {
		return nil, fmt.Errorf("journal: resolve export root: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("journal: inspect export root: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("journal: export root is not a directory")
	}
	committedSinks.Lock()
	defer committedSinks.Unlock()
	for existing := range committedSinks.roots {
		if pathWithin(existing, root) || pathWithin(root, existing) {
			return nil, errors.New("journal: committed event sink already registered for overlapping root")
		}
	}
	reg := &commitRegistration{sink: sink, instanceID: instanceID}
	reg.active.Store(true)
	committedSinks.roots[root] = reg
	committedSinks.count.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			committedSinks.Lock()
			delete(committedSinks.roots, root)
			committedSinks.count.Add(-1)
			reg.active.Store(false)
			reg.mu.Lock()
			reg.sink = nil
			reg.mu.Unlock()
			committedSinks.Unlock()
		})
	}, nil
}

// HasCommittedEventSink reports whether root already has a live owner.
func HasCommittedEventSink(root string) bool {
	return registeredCommitSink(root) != nil
}

func canonicalJournalPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("journal: path is empty")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
	}
	return filepath.Clean(path), nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func registeredCommitSink(dir string) *commitRegistration {
	if committedSinks.count.Load() == 0 {
		return nil
	}
	dir, err := canonicalJournalPath(dir)
	if err != nil {
		// Constructors already validate the directory for file writes. A
		// disappearing directory cannot safely be attributed to an export root.
		return nil
	}
	committedSinks.RLock()
	defer committedSinks.RUnlock()
	for root, reg := range committedSinks.roots {
		if pathWithin(root, dir) {
			return reg
		}
	}
	return nil
}

type commitTarget struct {
	reg     *commitRegistration
	context CommittedEvent
	pending *CommittedEvent
	staged  bool
}

func runCommitTarget(dir string, id RunIdentity) *commitTarget {
	reg := registeredCommitSink(dir)
	if reg == nil {
		return nil
	}
	instanceID := id.InstanceID
	if instanceID == "" {
		instanceID = reg.instanceID
	}
	return &commitTarget{reg: reg, context: CommittedEvent{
		Kind: "run", JournalID: id.RunID, RunID: id.RunID,
		InstanceID: instanceID, Gaggle: id.Gaggle,
	}}
}

func instanceCommitTarget(dir, id string) *commitTarget {
	reg := registeredCommitSink(dir)
	if reg == nil {
		return nil
	}
	return &commitTarget{reg: reg, context: CommittedEvent{
		Kind: "scheduler", JournalID: id, InstanceID: reg.instanceID,
	}}
}

func (t *commitTarget) notify(ev Event, line []byte) {
	if t == nil || !t.reg.active.Load() {
		return
	}
	event := t.context
	event.Seq, event.Time, event.ObservedTime = ev.Seq, ev.Time, time.Now()
	event.Body = append([]byte(nil), line...)
	if t.staged {
		t.pending = &event
		return
	}
	t.offer(event)
}

// committedSinkPanics counts sink panics contained by offer. A panicking sink is
// a bug in the sink, but it must never be a bug in the journal, so the count is
// the only trace it leaves here; CommittedSinkPanicCount exposes it so an owner
// can surface it rather than have it vanish.
var committedSinkPanics atomic.Uint64

// CommittedSinkPanicCount reports how many sink panics have been contained in
// this process.
func CommittedSinkPanicCount() uint64 { return committedSinkPanics.Load() }

func (t *commitTarget) offer(event CommittedEvent) {
	t.reg.mu.RLock()
	defer t.reg.mu.RUnlock()
	if t.reg.sink == nil {
		return
	}
	// Commit runs inside appendEvent, after the event is already durable and
	// with the journal's write lock (and, for an instance log, a cross-process
	// flock) held. A panicking sink must not unwind into the journal writer:
	// the event is committed, the caller's append succeeded, and telemetry is
	// not permitted to take down the durability substrate. The sink stays
	// registered — a panic may be data-dependent, and silently disabling export
	// would trade a loud failure for a quiet one.
	//
	// This goes through panicguard because recover is shadowed package-wide by
	// this package's own recover (reader.go).
	if panicguard.Call(t.reg.sink.Commit, event) {
		committedSinkPanics.Add(1)
	}
}

func (t *commitTarget) publish() {
	if t != nil && t.pending != nil {
		t.offer(*t.pending)
		t.pending = nil
		t.staged = false
	}
}
