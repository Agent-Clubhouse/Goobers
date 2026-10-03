package gagglehealth

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/platform/durability"
)

const maxHealthEventBytes = 256 << 10

// Store owns the instance-level health journal and its derived per-gaggle
// projections. The journal is authoritative; state files are replaceable caches.
type Store struct {
	root      string
	retention time.Duration
	now       func() time.Time

	mu     sync.Mutex
	events []apiv1.GaggleHealthEvent
}

// OpenStore opens an instance health store, validates its journal, and rebuilds
// every gaggle projection so restart recovery does not depend on state files.
func OpenStore(instanceRoot string, retention time.Duration) (*Store, error) {
	return openStore(instanceRoot, retention, time.Now)
}

func openStore(instanceRoot string, retention time.Duration, now func() time.Time) (*Store, error) {
	if !filepath.IsAbs(instanceRoot) {
		return nil, errors.New("gagglehealth: absolute instance root required")
	}
	if retention <= 0 || now == nil {
		return nil, errors.New("gagglehealth: positive evidence retention required")
	}
	store := &Store{root: filepath.Join(instanceRoot, "health"), retention: retention, now: now}
	if err := os.MkdirAll(store.root, 0o700); err != nil {
		return nil, fmt.Errorf("gagglehealth: create store: %w", err)
	}
	events, err := readEvents(store.eventsPath())
	if err != nil {
		return nil, err
	}
	store.events = events
	if err := store.rebuildAllLocked(); err != nil {
		return nil, err
	}
	return store, nil
}

// Append validates and durably appends one globally sequenced transition, then
// atomically replaces the affected gaggle's projection.
func (s *Store) Append(event apiv1.GaggleHealthEvent) (apiv1.GaggleHealthSnapshot, error) {
	if s == nil {
		return apiv1.GaggleHealthSnapshot{}, errors.New("gagglehealth: nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	wantSequence := uint64(1)
	if len(s.events) != 0 {
		wantSequence = s.events[len(s.events)-1].Sequence + 1
	}
	if event.Sequence != wantSequence {
		return apiv1.GaggleHealthSnapshot{}, fmt.Errorf("gagglehealth: event sequence %d, want %d", event.Sequence, wantSequence)
	}
	if err := validateStoreGaggle(event.Gaggle); err != nil {
		return apiv1.GaggleHealthSnapshot{}, err
	}
	candidate := append(append([]apiv1.GaggleHealthEvent(nil), s.events...), event)
	snapshot, err := s.project(event.Gaggle, candidate)
	if err != nil {
		return apiv1.GaggleHealthSnapshot{}, fmt.Errorf("gagglehealth: validate event: %w", err)
	}
	if err := appendEvent(s.eventsPath(), event); err != nil {
		return apiv1.GaggleHealthSnapshot{}, err
	}
	s.events = candidate
	if err := s.writeSnapshot(snapshot); err != nil {
		return apiv1.GaggleHealthSnapshot{}, err
	}
	return snapshot, nil
}

// Snapshot rebuilds one gaggle from the authoritative in-memory journal view.
func (s *Store) Snapshot(gaggle string) (apiv1.GaggleHealthSnapshot, error) {
	if s == nil {
		return apiv1.GaggleHealthSnapshot{}, errors.New("gagglehealth: nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateStoreGaggle(gaggle); err != nil {
		return apiv1.GaggleHealthSnapshot{}, err
	}
	return s.project(gaggle, s.events)
}

func (s *Store) rebuildAllLocked() error {
	gaggles := map[string]struct{}{}
	var previous uint64
	for _, event := range s.events {
		if event.Sequence <= previous {
			return errors.New("gagglehealth: journal sequence is not strictly increasing")
		}
		previous = event.Sequence
		if err := validateStoreGaggle(event.Gaggle); err != nil {
			return err
		}
		gaggles[event.Gaggle] = struct{}{}
	}
	names := make([]string, 0, len(gaggles))
	for gaggle := range gaggles {
		names = append(names, gaggle)
	}
	sort.Strings(names)
	for _, gaggle := range names {
		snapshot, err := s.project(gaggle, s.events)
		if err != nil {
			return fmt.Errorf("gagglehealth: rebuild %q: %w", gaggle, err)
		}
		if err := s.writeSnapshot(snapshot); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) project(gaggle string, events []apiv1.GaggleHealthEvent) (apiv1.GaggleHealthSnapshot, error) {
	partition := make([]apiv1.GaggleHealthEvent, 0)
	for _, event := range events {
		if event.Gaggle == gaggle {
			partition = append(partition, event)
		}
	}
	return RebuildWithRetention(gaggle, partition, s.retention, s.now())
}

func (s *Store) eventsPath() string {
	return filepath.Join(s.root, "events.jsonl")
}

func (s *Store) writeSnapshot(snapshot apiv1.GaggleHealthSnapshot) error {
	dir := filepath.Join(s.root, "gaggles", snapshot.Gaggle)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("gagglehealth: create projection directory: %w", err)
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("gagglehealth: encode projection: %w", err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("gagglehealth: create projection temp file: %w", err)
	}
	tempPath := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}
	if err := temp.Chmod(0o600); err != nil {
		cleanup()
		return fmt.Errorf("gagglehealth: secure projection temp file: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("gagglehealth: write projection: %w", err)
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("gagglehealth: sync projection: %w", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("gagglehealth: close projection: %w", err)
	}
	path := filepath.Join(dir, "state.json")
	if err := durability.ReplaceFile(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("gagglehealth: replace projection: %w", err)
	}
	if err := durability.SyncDir(dir); err != nil {
		return fmt.Errorf("gagglehealth: sync projection directory: %w", err)
	}
	return nil
}

func appendEvent(path string, event apiv1.GaggleHealthEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("gagglehealth: encode event: %w", err)
	}
	if len(data) > maxHealthEventBytes {
		return errors.New("gagglehealth: event exceeds size limit")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("gagglehealth: open journal: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return fmt.Errorf("gagglehealth: append journal: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("gagglehealth: sync journal: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("gagglehealth: close journal: %w", err)
	}
	if err := durability.SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("gagglehealth: sync journal directory: %w", err)
	}
	return nil
}

func readEvents(path string) ([]apiv1.GaggleHealthEvent, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gagglehealth: open journal: %w", err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxHealthEventBytes)
	var events []apiv1.GaggleHealthEvent
	for scanner.Scan() {
		var event apiv1.GaggleHealthEvent
		decoder := json.NewDecoder(strings.NewReader(scanner.Text()))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&event); err != nil {
			return nil, fmt.Errorf("gagglehealth: decode journal event %d: %w", len(events)+1, err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("gagglehealth: read journal: %w", err)
	}
	return events, nil
}

func validateStoreGaggle(gaggle string) error {
	if err := validateIdentifier("gaggle partition", gaggle, true); err != nil {
		return err
	}
	if strings.ContainsAny(gaggle, `/\`) || gaggle == "." || gaggle == ".." {
		return errors.New("gagglehealth: gaggle partition is not a safe path segment")
	}
	return nil
}
