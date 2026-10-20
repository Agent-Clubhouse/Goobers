package interactiveauth

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/platform/lock"
)

type authorityFence struct {
	Epoch uint64 `json:"epoch"`
	State string `json:"state"`
}
type persisted struct {
	Schema      int                       `json:"schema"`
	Connections map[string]Connection     `json:"connections"`
	Authorities map[string]Authority      `json:"authorities"`
	Fences      map[string]authorityFence `json:"fences"`
	Budgets     map[string]Budget         `json:"budgets"`
	Decisions   map[string]Decision       `json:"decisions"`
	Operations  map[string]Operation      `json:"operations"`
	// DemoComments represents the controlled provider's durable backing service.
	// It lets reconciliation find an effect after the broker loses its response.
	DemoComments map[string]map[string]any `json:"demoComments"`
}

// No bearer/refresh token field belongs in persisted, public DTOs or events.
// Real credential custody and Microsoft acquisition intentionally remain unimplemented.
type availability struct{ refreshable bool }

// Broker mediates bounded provider access and persists operation receipts.
type Broker struct {
	mu             sync.Mutex
	data           persisted
	path           string
	now            func() time.Time
	checkExecution func(context.Context, Identity, OperationRequest) error
	lock           *lock.Handle
	closed         bool
	storageFailed  bool
	availability   map[string]availability
	// Tests exercise revocation in the admission-to-send window. This callback
	// is private; production callers cannot bypass the typed provider path.
	afterAdmission func()
}

// Open loads the controlled broker and its durable state.
func Open(cfg Config) (*Broker, error) {
	b := &Broker{path: cfg.Path, now: cfg.Now, checkExecution: cfg.CheckExecution, availability: map[string]availability{}}
	if b.now == nil {
		b.now = time.Now
	}
	b.data = persisted{Schema: 1, Connections: map[string]Connection{}, Authorities: map[string]Authority{}, Fences: map[string]authorityFence{}, Budgets: map[string]Budget{}, Decisions: map[string]Decision{}, Operations: map[string]Operation{}, DemoComments: map[string]map[string]any{}}
	if cfg.Path != "" {
		if err := os.MkdirAll(filepath.Dir(cfg.Path), 0700); err != nil {
			return nil, fault("unavailable", "cannot create authority store directory")
		}
		var err error
		b.lock, err = lock.TryAcquire(cfg.Path + ".lock")
		if err != nil {
			return nil, fault("unavailable", "authority store is already open or cannot be locked")
		}
		contents, err := os.ReadFile(cfg.Path)
		if err == nil {
			if json.Unmarshal(contents, &b.data) != nil || b.data.Schema != 1 || b.data.Connections == nil || b.data.Authorities == nil || b.data.Fences == nil || b.data.Budgets == nil || b.data.Decisions == nil || b.data.Operations == nil || b.data.DemoComments == nil {
				_ = b.lock.Release()
				return nil, fault("unavailable", "authority store is corrupt or unsupported")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			_ = b.lock.Release()
			return nil, fault("unavailable", "cannot read authority store")
		}
	}
	changed := false
	for id, c := range b.data.Connections {
		// A restarted controlled broker has no durable acquisition context.
		// It visibly requires the same owner to reauthenticate; never assume renewal.
		if c.State != "revoked" {
			c.State = "interaction_required"
			b.data.Connections[id] = c
			changed = true
		}
	}
	for id, op := range b.data.Operations {
		if op.State == "dispatched" {
			op.State = "outcome_unknown"
			op.ErrorCode = "outcome_unknown"
			op.Message = "Broker restarted after dispatch; reconcile the original operation before further action."
			b.data.Operations[id] = op
			changed = true
		}
	}
	if changed {
		if err := b.persistLocked(); err != nil {
			_ = b.lock.Release()
			return nil, err
		}
	}
	return b, nil
}

// Close persists the broker’s current state.
func (b *Broker) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if b.lock != nil {
		return b.lock.Release()
	}
	return nil
}

func (b *Broker) readyLocked(p Principal) error {
	if !p.verified || p.identity.Issuer != "goobers:controlled-local" || p.identity.Subject == "" {
		return fault("unauthenticated", "a verified local prototype principal is required")
	}
	if b.closed || b.storageFailed {
		return fault("unavailable", "authority store is unavailable; no effect can be admitted")
	}
	return nil
}

func (b *Broker) persistLocked() error {
	if b.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(b.data, "", "  ")
	if err != nil {
		b.storageFailed = true
		return fault("unavailable", "authority state could not be encoded")
	}
	f, err := os.CreateTemp(filepath.Dir(b.path), ".interactive-auth-*")
	if err != nil {
		b.storageFailed = true
		return fault("unavailable", "authority store could not be written")
	}
	name := f.Name()
	// Best-effort cleanup; the operation result is reported separately.
	defer func() { _ = os.Remove(name) }()
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, b.path)
	}
	if err == nil {
		dir, openErr := os.Open(filepath.Dir(b.path))
		err = openErr
		if err == nil {
			err = dir.Sync()
			_ = dir.Close()
		}
	}
	if err != nil {
		b.storageFailed = true
		return fault("unavailable", "authority store write is uncertain; broker fenced until recovery")
	}
	return nil
}

// clone prevents callers from mutating the broker's immutable records through slices/maps.
func clone[T any](v T) T {
	data, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(data, &out)
	return out
}
