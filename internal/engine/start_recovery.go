package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/goobers/goobers/internal/livejournal"
)

// ErrStartOwnershipUncertain quarantines only the affected workflow lane.
var ErrStartOwnershipUncertain = errors.New("start ownership uncertain")

// PendingStartIntents reads provisional custody without claiming ownership.
func PendingStartIntents(directory string) ([]StartIntent, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var intents []StartIntent
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var intent StartIntent
		if err := json.Unmarshal(raw, &intent); err != nil {
			return nil, err
		}
		if entry.Name() != filepath.Base(intentPath(directory, intent.Input.RunID)) {
			return nil, errors.New("engine: provisional custody run ID differs from filename")
		}
		intents = append(intents, intent)
	}
	return intents, nil
}

// RecoverStartIntents schedules independent bounded retries. An ambiguous run
// stays quarantined without blocking startup or recovery of unrelated runs.
// settle attaches only after classification and holds the lane through terminal.
func RecoverStartIntents(ctx context.Context, directory string, starter Starter, live *livejournal.Writer, settle func(context.Context, RunInput) error) error {
	intents, err := PendingStartIntents(directory)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		go retryStartIntent(ctx, directory, intent, starter, live, settle)
	}
	return nil
}

func retryStartIntent(ctx context.Context, directory string, intent StartIntent, starter Starter, live *livejournal.Writer, settle func(context.Context, RunInput) error) {
	delay := time.Second
	for ctx.Err() == nil {
		if err := recoverStartIntent(ctx, directory, intent, starter, live, settle); err == nil {
			return
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = min(delay*2, time.Minute)
	}
}

func recoverStartIntent(ctx context.Context, directory string, intent StartIntent, starter Starter, live *livejournal.Writer, settle func(context.Context, RunInput) error) error {
	attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
	result, err := starter.Start(attempt, intent.Input)
	cancel()
	if err != nil {
		return err
	}
	if result.NotMine {
		return ReleaseStart(directory, intent.Input.RunID)
	}
	if live == nil {
		return errors.New("engine: recovery requires a live journal")
	}
	if err := publishStart(ctx, intent, live); err != nil {
		return err
	}
	if err := settle(ctx, intent.Input); err != nil {
		return err
	}
	return ReleaseStart(directory, intent.Input.RunID)
}
