package interactive

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type storedCommand struct {
	Digest  string         `json:"digest"`
	Receipt CommandReceipt `json:"receipt"`
}
type storedCreate struct {
	Digest    string `json:"digest"`
	SessionID string `json:"sessionId"`
}
type sessionRecord struct {
	Session  Session                  `json:"session"`
	Events   []Event                  `json:"events"`
	Commands map[string]storedCommand `json:"commands"`
}
type diskState struct {
	Version  int                       `json:"version"`
	Sessions map[string]*sessionRecord `json:"sessions"`
	Creates  map[string]storedCreate   `json:"creates"`
}

func emptyState() diskState {
	return diskState{Version: 1, Sessions: map[string]*sessionRecord{}, Creates: map[string]storedCreate{}}
}
func clone[T any](value T) T {
	b, _ := json.Marshal(value)
	var copied T
	_ = json.Unmarshal(b, &copied)
	return copied
}

// persist writes a complete transaction with restrictive permissions, fsyncs it,
// atomically replaces the snapshot, then syncs the containing directory. The
// process lock ensures there is exactly one coordinator for this state dir.
func (s *Service) persist(state diskState) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.opts.StateDir, ".sessions-*.tmp")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(name, filepath.Join(s.opts.StateDir, "sessions.json")); err != nil {
		return err
	}
	dir, err := os.Open(s.opts.StateDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// mutate requires the coordinator mutex. No state or receipt becomes visible
// until its durable write succeeds. Failed writes leave the prior state intact.
func (s *Service) mutate(fn func(*diskState) error) error {
	if s.fatal != nil {
		return problem(503, "storage_unavailable", "session persistence requires recovery")
	}
	next := clone(s.state)
	if err := fn(&next); err != nil {
		return err
	}
	if err := s.persist(next); err != nil {
		// A directory sync error can follow a successful rename. Freeze admission
		// rather than issuing effects from an uncertain persistence outcome.
		s.fatal = fmt.Errorf("persist interactive session: %w", err)
		for _, runs := range s.active {
			for _, run := range runs {
				run.cancel()
			}
		}
		return problem(503, "storage_unavailable", "session persistence failed; admission stopped")
	}
	s.state = next
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}
func appendEvent(rec *sessionRecord, kind, turnID, commandID string, data map[string]any) Event {
	rec.Session.Sequence++
	now := time.Now().UTC()
	rec.Session.UpdatedAt = now
	if data == nil {
		data = map[string]any{}
	}
	event := Event{Actor: rec.Session.Owner, Sequence: rec.Session.Sequence, SessionID: rec.Session.ID, TurnID: turnID, CommandID: commandID, Type: kind, Time: now, Data: data}
	rec.Events = append(rec.Events, event)
	return event
}
