package baseline

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Observation is one measurement of a CI command against a repository's target
// branch at a pinned base SHA. It is cached so the second and every later run
// that syncs the same base pays nothing to learn the base's health.
type Observation struct {
	Repo        string    `json:"repo"`
	BaseSHA     string    `json:"baseSha"`
	Command     string    `json:"command"`
	Green       bool      `json:"green"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	Signature   string    `json:"signature,omitempty"`
	ObservedAt  time.Time `json:"observedAt"`
}

// Waiter is one subject (backlog item or pull request) parked on a shared
// blocker, recorded with the base SHA it was parked at so the retry decision
// can tell "still on the same red base" from "the base has advanced".
type Waiter struct {
	Subject  string    `json:"subject"`
	RunID    string    `json:"runId,omitempty"`
	BaseSHA  string    `json:"baseSha"`
	ParkedAt time.Time `json:"parkedAt"`
}

// Blocker is the one durable record per distinct shared baseline failure. Every
// affected run parks against the same Blocker instead of opening its own.
type Blocker struct {
	Key         string    `json:"key"`
	Repo        string    `json:"repo"`
	Command     string    `json:"command"`
	Fingerprint string    `json:"fingerprint"`
	Signature   string    `json:"signature"`
	FirstSeenAt time.Time `json:"firstSeenAt"`
	LastSeenAt  time.Time `json:"lastSeenAt"`
	// BaseSHAs are the pinned base commits this failure was observed at, in
	// first-seen order; the last entry is the most recent red base.
	BaseSHAs []string `json:"baseShas"`
	Resolved bool     `json:"resolved"`
	Waiting  []Waiter `json:"waiting,omitempty"`
}

// state is the store's on-disk form.
type state struct {
	Version      int                    `json:"version"`
	Observations map[string]Observation `json:"observations"`
	Blockers     map[string]*Blocker    `json:"blockers"`
}

const stateVersion = 1

// Store is the durable, process-shared record of baseline observations and
// shared blockers. It is safe for concurrent use; every mutation is persisted
// with an atomic replace so a crash mid-write cannot leave a half-written file.
type Store struct {
	path string

	mu    sync.Mutex
	state state
}

// OpenStore loads (or initializes) the store at path.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, state: state{Version: stateVersion, Observations: map[string]Observation{}, Blockers: map[string]*Blocker{}}}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
	case os.IsNotExist(err):
		return s, nil
	default:
		return nil, fmt.Errorf("baseline: read store %q: %w", path, err)
	}
	var loaded state
	if err := json.Unmarshal(data, &loaded); err != nil {
		return nil, fmt.Errorf("baseline: parse store %q: %w", path, err)
	}
	if loaded.Observations != nil {
		s.state.Observations = loaded.Observations
	}
	if loaded.Blockers != nil {
		s.state.Blockers = loaded.Blockers
	}
	return s, nil
}

// Baseline returns the cached observation for repo's target branch at baseSHA
// for command, if one exists.
func (s *Store) Baseline(repo, baseSHA string, command []string) (Observation, bool) {
	if s == nil {
		return Observation{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	observation, ok := s.state.Observations[observationKey(repo, baseSHA, CommandKey(command))]
	return observation, ok
}

// Record stores a baseline observation. A green observation also resolves any
// blocker whose failure the base no longer reproduces, releasing its waiters.
func (s *Store) Record(observation Observation) error {
	if s == nil {
		return ErrNoStore
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Observations[observationKey(observation.Repo, observation.BaseSHA, observation.Command)] = observation
	if observation.Green {
		for _, blocker := range s.state.Blockers {
			if blocker.Repo != observation.Repo || blocker.Command != observation.Command || blocker.Resolved {
				continue
			}
			if staleResolution(blocker, observation) {
				continue
			}
			blocker.Resolved = true
		}
	}
	return s.persistLocked()
}

// staleResolution reports that a green observation is not evidence about the
// base the blocker is currently red at: it was measured before the blocker was
// last seen red, or it is pinned to a base commit the blocker has already moved
// past. Resolving on either would release every waiter onto a base that still
// reproduces the failure.
func staleResolution(blocker *Blocker, observation Observation) bool {
	if observation.ObservedAt.Before(blocker.LastSeenAt) {
		return true
	}
	if len(blocker.BaseSHAs) > 1 && contains(blocker.BaseSHAs[:len(blocker.BaseSHAs)-1], observation.BaseSHA) {
		return true
	}
	return false
}

// Park records waiter against the durable blocker for observation's failure,
// creating that blocker on first sight. Re-parking the same subject refreshes
// its entry rather than adding a duplicate.
func (s *Store) Park(observation Observation, waiter Waiter) (Blocker, error) {
	if s == nil {
		return Blocker{}, ErrNoStore
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := BlockerKey(observation.Repo, observation.Fingerprint)
	blocker, ok := s.state.Blockers[key]
	if !ok {
		blocker = &Blocker{
			Key:         key,
			Repo:        observation.Repo,
			Command:     observation.Command,
			Fingerprint: observation.Fingerprint,
			Signature:   observation.Signature,
			FirstSeenAt: waiter.ParkedAt,
		}
		s.state.Blockers[key] = blocker
	}
	blocker.Resolved = false
	blocker.LastSeenAt = waiter.ParkedAt
	if !contains(blocker.BaseSHAs, observation.BaseSHA) {
		blocker.BaseSHAs = append(blocker.BaseSHAs, observation.BaseSHA)
	}
	if waiter.Subject != "" {
		replaced := false
		for i := range blocker.Waiting {
			if blocker.Waiting[i].Subject == waiter.Subject {
				blocker.Waiting[i] = waiter
				replaced = true
				break
			}
		}
		if !replaced {
			blocker.Waiting = append(blocker.Waiting, waiter)
		}
	}
	if err := s.persistLocked(); err != nil {
		return Blocker{}, err
	}
	return *blocker, nil
}

// Blockers returns every recorded blocker for repo (all repositories when repo
// is empty), ordered by key so operator output is stable.
func (s *Store) Blockers(repo string) []Blocker {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Blocker, 0, len(s.state.Blockers))
	for _, blocker := range s.state.Blockers {
		if repo != "" && blocker.Repo != repo {
			continue
		}
		out = append(out, *blocker)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ReadyToRetry returns the parked subjects that may run again: those whose
// blocker is resolved (the baseline recovered) and those parked at a base SHA
// the target branch has since advanced past — a new base is new evidence, so
// the run is retried rather than left parked on a stale observation.
func (s *Store) ReadyToRetry(repo, currentBaseSHA string) []Waiter {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var ready []Waiter
	for _, blocker := range s.state.Blockers {
		if blocker.Repo != repo {
			continue
		}
		for _, waiter := range blocker.Waiting {
			if blocker.Resolved || (currentBaseSHA != "" && waiter.BaseSHA != currentBaseSHA) {
				ready = append(ready, waiter)
			}
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Subject < ready[j].Subject })
	return ready
}

// Release drops subject from every blocker in repo — the counterpart to Park,
// called once a released subject has actually been retried.
func (s *Store) Release(repo, subject string) error {
	if s == nil {
		return ErrNoStore
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, blocker := range s.state.Blockers {
		if blocker.Repo != repo {
			continue
		}
		kept := blocker.Waiting[:0]
		for _, waiter := range blocker.Waiting {
			if waiter.Subject != subject {
				kept = append(kept, waiter)
			}
		}
		blocker.Waiting = kept
	}
	return s.persistLocked()
}

func (s *Store) persistLocked() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("baseline: encode store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return fmt.Errorf("baseline: create store directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".tmp*")
	if err != nil {
		return fmt.Errorf("baseline: create store temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("baseline: write store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("baseline: sync store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("baseline: close store: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("baseline: replace store: %w", err)
	}
	return nil
}

// BlockerKey is the durable identity of one shared baseline failure.
func BlockerKey(repo, fingerprint string) string {
	return repo + "@" + short(fingerprint)
}

func observationKey(repo, baseSHA, command string) string {
	return repo + "\x00" + baseSHA + "\x00" + command
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
