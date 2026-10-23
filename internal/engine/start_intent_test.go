package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

type intentStarter func(context.Context, RunInput) (StartResult, error)

func (f intentStarter) Start(ctx context.Context, in RunInput) (StartResult, error) {
	return f(ctx, in)
}

func TestStartIntentReplayPinsInputAndQuarantinesOnlyItsWorkflow(t *testing.T) {
	dir := t.TempDir()
	in := sampleInput()
	first, err := ProvisionStart(dir, in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	replay, err := ProvisionStart(dir, in, time.Now().Add(time.Minute))
	if err != nil || !first.ReservedAt.Equal(replay.ReservedAt) {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	changed := in
	changed.Version++
	if _, err := ProvisionStart(dir, changed, time.Now()); err == nil {
		t.Fatal("changed provisional input accepted")
	}
	changed = in
	changed.RunID = "another-run"
	if _, err := ProvisionStart(dir, changed, time.Now()); !errors.Is(err, ErrStartOwnershipUncertain) {
		t.Fatalf("same workflow quarantine = %v", err)
	}
	changed.WorkflowName = "another-workflow"
	if _, err := ProvisionStart(dir, changed, time.Now()); err != nil {
		t.Fatalf("unrelated workflow blocked: %v", err)
	}
}

func TestRecoveryNotMineReleasesOnlyProvisionalCustody(t *testing.T) {
	dir := t.TempDir()
	in := sampleInput()
	in.RunID = "recovery-run"
	intent, err := ProvisionStart(dir, in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	starter := intentStarter(func(context.Context, RunInput) (StartResult, error) { return StartResult{NotMine: true}, nil })
	if err := recoverStartIntent(t.Context(), dir, intent, starter, nil, func(context.Context, RunInput) error { t.Fatal("NotMine awaited"); return nil }); err != nil {
		t.Fatal(err)
	}
	pending, err := PendingStartIntents(dir)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
}

func TestRecoveryConfirmsOnlyAfterClassificationAndHoldsQuarantineUntilSettled(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "intents")
	runs := filepath.Join(root, "runs")
	live, err := livejournal.NewWriter(func(string) (string, bool) { return runs, true })
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	in := sampleInput()
	in.RunID = "recovery-run"
	intent, err := ProvisionStart(dir, in, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	starter := intentStarter(func(context.Context, RunInput) (StartResult, error) {
		if _, err := os.Stat(filepath.Join(runs, in.RunID)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("journal before classification")
		}
		return StartResult{RunID: in.RunID, AlreadyRunning: true, Adopt: true}, nil
	})
	settle := func(context.Context, RunInput) error {
		if _, err := journal.OpenRead(filepath.Join(runs, in.RunID)); err != nil {
			t.Fatal(err)
		}
		pending, err := PendingStartIntents(dir)
		if err != nil || len(pending) != 1 {
			t.Fatalf("quarantine released before settlement: %+v,%v", pending, err)
		}
		return nil
	}
	if err := recoverStartIntent(t.Context(), dir, intent, starter, live, settle); err != nil {
		t.Fatal(err)
	}
	pending, err := PendingStartIntents(dir)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after settlement = %+v,%v", pending, err)
	}
}

func TestUncertainStartDoesNotBlockStartupOrOtherRecoveryAndRetries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"uncertain", "other"} {
		in := sampleInput()
		in.RunID = name
		in.WorkflowName = name
		if _, err := ProvisionStart(dir, in, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	var attempts atomic.Int32
	other := make(chan struct{})
	retried := make(chan struct{})
	starter := intentStarter(func(_ context.Context, in RunInput) (StartResult, error) {
		if in.RunID == "other" {
			close(other)
			return StartResult{NotMine: true}, nil
		}
		if attempts.Add(1) == 2 {
			close(retried)
		}
		return StartResult{}, errors.New("ownership unavailable")
	})
	if err := RecoverStartIntents(t.Context(), dir, starter, nil, nil); err != nil {
		t.Fatalf("startup blocked: %v", err)
	}
	select {
	case <-other:
	case <-time.After(5 * time.Second):
		t.Fatal("unrelated recovery blocked")
	}
	select {
	case <-retried:
	case <-time.After(5 * time.Second):
		t.Fatal("uncertain start was not retried")
	}
	pending, err := PendingStartIntents(dir)
	if err != nil || len(pending) != 1 || pending[0].Input.RunID != "uncertain" {
		t.Fatalf("uncertain custody lost: %+v,%v", pending, err)
	}
}
