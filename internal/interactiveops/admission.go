package interactiveops

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/lock"
)

// ErrCapacity reports exhausted or unavailable helper worker capacity.
var ErrCapacity = errors.New("helper execution capacity unavailable")

// reserveWorker serializes admission across host processes sharing Root. OS
// worker locks reserve live capacity; an abandoned dispatched operation whose
// cleanup is unconfirmed retains its slot until explicit custody reconciliation.
func (s *Service) reserveWorker(scope Scope, dir string) (*lock.Handle, error) {
	admission, err := lock.TryAcquire(filepath.Join(s.opts.Root, "admission.lock"))
	if err != nil {
		return nil, fmt.Errorf("%w: admission is busy", ErrCapacity)
	}
	// Best-effort cleanup; the operation result is reported separately.
	defer func() { _ = admission.Release() }()
	entries, err := os.ReadDir(s.opts.Root)
	if err != nil {
		return nil, err
	}
	active, ownerActive := 0, 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		other := filepath.Join(s.opts.Root, entry.Name())
		b, err := os.ReadFile(filepath.Join(other, "owner.json"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var owner Scope
		if err = json.Unmarshal(b, &owner); err != nil {
			return nil, err
		}
		worker, err := lock.TryAcquire(filepath.Join(other, "worker.lock"))
		occupied := errors.Is(err, lock.ErrHeld)
		if err != nil && !occupied {
			return nil, err
		}
		if worker != nil {
			_ = worker.Release()
		}
		if !occupied {
			actions, err := allActions(other)
			if err != nil {
				return nil, err
			}
			for _, a := range actions {
				if a.Kind == "submit_run" && !a.CleanupConfirmed && (a.OperationStatus == "dispatched" || a.OperationStatus == "outcome_unknown" || a.OperationStatus == "confirmed") {
					occupied = true
					break
				}
			}
		}
		if occupied {
			if other == dir {
				return nil, fmt.Errorf("%w: this session already has a worker or unresolved custody", ErrCapacity)
			}
			active++
			if owner.ActorID == scope.ActorID {
				ownerActive++
			}
		}
	}
	if active >= s.opts.MaxActive || ownerActive >= s.opts.MaxActivePerOwner {
		return nil, fmt.Errorf("%w: active %d/%d, actor %d/%d; retry this proposal after capacity releases", ErrCapacity, active, s.opts.MaxActive, ownerActive, s.opts.MaxActivePerOwner)
	}
	worker, err := lock.TryAcquire(filepath.Join(dir, "worker.lock"))
	if err != nil {
		return nil, fmt.Errorf("%w: session worker is busy", ErrCapacity)
	}
	return worker, nil
}
