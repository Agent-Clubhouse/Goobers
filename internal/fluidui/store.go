package fluidui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/platform/durability"
)

// MaxHistory bounds how many earlier revisions undo can restore.
const MaxHistory = 20

// LocalUser is the user key for anonymous, loopback-trusted dashboards.
const LocalUser = "local:"

// recordSchema versions the on-disk record, independent of Version.
const recordSchema = 1

// ErrNothingToUndo is returned by Undo when no earlier revision is retained.
var ErrNothingToUndo = errors.New("fluidui: no earlier customization to restore")

// State is what a user sees of their own customization.
type State struct {
	// Customization is nil when the standard Portal UI applies.
	Customization *Customization `json:"customization"`
	Revision      int64          `json:"revision"`
	UpdatedAt     *time.Time     `json:"updatedAt,omitempty"`
	// Incompatible is set when the stored document no longer validates
	// (for example, written by a newer build). The Portal falls back to the
	// standard UI and the user can reset or undo.
	Incompatible bool `json:"incompatible,omitempty"`
	UndoDepth    int  `json:"undoDepth"`
}

type revision struct {
	// Document is the raw customization; null means the standard UI.
	Document  json.RawMessage `json:"document"`
	Revision  int64           `json:"revision"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

type record struct {
	Schema  int        `json:"schema"`
	Current revision   `json:"current"`
	History []revision `json:"history,omitempty"`
}

// Store persists one record per user under Dir.
type Store struct {
	Dir string
	Now func() time.Time
	mu  sync.Mutex
}

// NewStore roots a store at dir.
func NewStore(dir string) *Store { return &Store{Dir: dir} }

// ForInstance returns the store kept under an instance root.
func ForInstance(root string) *Store { return NewStore(filepath.Join(root, "labs", "fluid-ui")) }

// UserKey identifies a user by authentication issuer and subject. An empty
// subject is the anonymous local user.
func UserKey(issuer, subject string) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return LocalUser
	}
	return strings.TrimSpace(issuer) + ":" + subject
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Store) path(user string) string {
	sum := sha256.Sum256([]byte(user))
	return filepath.Join(s.Dir, hex.EncodeToString(sum[:16])+".json")
}

// Get returns user's current customization state.
func (s *Store) Get(user string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.load(user)
	if err != nil {
		return State{}, err
	}
	return rec.state(), nil
}

// Put validates doc and makes it user's current customization.
func (s *Store) Put(user string, doc Customization) (State, error) {
	if err := doc.Validate(); err != nil {
		return State{}, err
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return State{}, fmt.Errorf("fluidui: encode customization: %w", err)
	}
	return s.replace(user, data)
}

// Reset returns user to the standard UI. The previous customization stays in
// history, so Undo restores it.
func (s *Store) Reset(user string) (State, error) {
	return s.replace(user, json.RawMessage("null"))
}

// Undo restores user's previous revision.
func (s *Store) Undo(user string) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.load(user)
	if err != nil {
		return State{}, err
	}
	if len(rec.History) == 0 {
		return State{}, ErrNothingToUndo
	}
	previous := rec.History[len(rec.History)-1]
	rec.History = rec.History[:len(rec.History)-1]
	rec.Current = revision{Document: previous.Document, Revision: rec.Current.Revision + 1, UpdatedAt: s.now()}
	if err := s.save(user, rec); err != nil {
		return State{}, err
	}
	return rec.state(), nil
}

func (s *Store) replace(user string, document json.RawMessage) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.load(user)
	if err != nil {
		return State{}, err
	}
	if rec.Current.Revision > 0 {
		rec.History = append(rec.History, rec.Current)
		if len(rec.History) > MaxHistory {
			rec.History = append([]revision(nil), rec.History[len(rec.History)-MaxHistory:]...)
		}
	}
	rec.Current = revision{Document: document, Revision: rec.Current.Revision + 1, UpdatedAt: s.now()}
	if err := s.save(user, rec); err != nil {
		return State{}, err
	}
	return rec.state(), nil
}

func (s *Store) load(user string) (record, error) {
	data, err := os.ReadFile(s.path(user))
	if errors.Is(err, fs.ErrNotExist) {
		return record{Schema: recordSchema}, nil
	}
	if err != nil {
		return record{}, fmt.Errorf("fluidui: read customization: %w", err)
	}
	var rec record
	if err := json.Unmarshal(data, &rec); err != nil {
		return record{}, fmt.Errorf("fluidui: decode customization record: %w", err)
	}
	if rec.Schema != recordSchema {
		return record{}, fmt.Errorf("fluidui: unsupported customization record schema %d", rec.Schema)
	}
	return rec, nil
}

func (s *Store) save(user string, rec record) error {
	rec.Schema = recordSchema
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("fluidui: encode customization record: %w", err)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("fluidui: create store: %w", err)
	}
	if err := durability.WriteFileAtomic(s.path(user), data, 0o600); err != nil {
		return fmt.Errorf("fluidui: write customization: %w", err)
	}
	return nil
}

func (rec record) state() State {
	state := State{Revision: rec.Current.Revision, UndoDepth: len(rec.History)}
	if rec.Current.Revision > 0 {
		updated := rec.Current.UpdatedAt
		state.UpdatedAt = &updated
	}
	if len(rec.Current.Document) == 0 || string(rec.Current.Document) == "null" {
		return state
	}
	doc, err := Decode(rec.Current.Document)
	if err != nil {
		state.Incompatible = true
		return state
	}
	state.Customization = &doc
	return state
}
