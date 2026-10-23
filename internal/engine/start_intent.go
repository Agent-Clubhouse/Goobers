package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/platform/durability"
	"github.com/goobers/goobers/internal/platform/secfile"
)

// StartIntent is durable provisional custody, not an owned run or journal.
// Recovery must repeat the start fence before attaching to its execution.
type StartIntent struct {
	Input      RunInput  `json:"input"`
	ReservedAt time.Time `json:"reservedAt"`
}

// Local daemon admission is process-exclusive; serialize duplicate intent writes.
var startIntentMu sync.Mutex

func intentPath(directory, runID string) string {
	return filepath.Join(directory, RunID(runID)+".json")
}

// ProvisionStart durably pins the input before Temporal can accept it. Replays
// keep the original timestamp and reject changes to the pinned input.
func ProvisionStart(directory string, in RunInput, at time.Time) (StartIntent, error) {
	startIntentMu.Lock()
	defer startIntentMu.Unlock()
	if in.RunID == "" {
		return StartIntent{}, errors.New("engine: provisional start requires a run ID")
	}
	pending, err := PendingStartIntents(directory)
	if err != nil {
		return StartIntent{}, err
	}
	for _, other := range pending {
		if other.Input.RunID != in.RunID && other.Input.Gaggle == in.Gaggle && other.Input.WorkflowName == in.WorkflowName {
			return StartIntent{}, ErrStartOwnershipUncertain
		}
	}
	intent := StartIntent{Input: in, ReservedAt: at}
	path := intentPath(directory, in.RunID)
	raw, err := os.ReadFile(path)
	if err == nil {
		var prior StartIntent
		if err := json.Unmarshal(raw, &prior); err != nil {
			return StartIntent{}, err
		}
		want, err := startOwner(in)
		if err != nil {
			return StartIntent{}, err
		}
		got, err := startOwner(prior.Input)
		if err != nil {
			return StartIntent{}, err
		}
		if got.InputDigest != want.InputDigest {
			return StartIntent{}, errors.New("engine: provisional start input changed")
		}
		return prior, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return StartIntent{}, err
	}
	raw, err = json.Marshal(intent)
	if err != nil {
		return StartIntent{}, err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return StartIntent{}, err
	}
	if err := durability.SyncDir(filepath.Dir(directory)); err != nil {
		return StartIntent{}, err
	}
	return intent, secfile.WritePrivateAtomic(path, raw)
}

// ReleaseStart removes provisional custody without authoring a run journal.
func ReleaseStart(directory, runID string) error {
	err := durability.RemoveFile(intentPath(directory, runID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return durability.SyncDir(directory)
}

// ConfirmStart publishes the idempotent owned-run reservation after the fence.
// Keep the intent on publication failure so recovery reclassifies it.
func ConfirmStart(ctx context.Context, directory string, intent StartIntent, live *livejournal.Writer) error {
	if live == nil {
		return errors.New("engine: confirming provisional custody requires a live journal")
	}
	if err := publishStart(ctx, intent, live); err != nil {
		return err
	}
	return ReleaseStart(directory, intent.Input.RunID)
}

func publishStart(ctx context.Context, intent StartIntent, live *livejournal.Writer) error {
	reservation, err := ReserveRun(intent.Input, intent.ReservedAt)
	if err != nil {
		return err
	}
	if _, err := live.Emit(ctx, reservation); err != nil {
		return err
	}
	return nil
}
