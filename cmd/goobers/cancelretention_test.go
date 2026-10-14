package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

func TestDeferredFamilyCancellationKeepsUnrelatedFailuresVisible(t *testing.T) {
	held := fmt.Errorf("parent result still held: %w", invoke.ErrChildCustodyPending)
	busy := fmt.Errorf("cleanup: %w: %w", worktree.ErrCleanupDeferred, journal.ErrRecoveryBusy)
	for _, test := range []struct {
		name  string
		phase journal.RunPhase
		err   error
		want  bool
	}{
		{"pending", journal.PhaseAborted, held, true},
		{"owned cleanup", journal.PhaseAborted, errors.Join(held, busy), true},
		{"still running", journal.PhaseRunning, held, false},
		{"escalated", journal.PhaseEscalated, held, false},
		{"ordinary cleanup", journal.PhaseAborted, busy, false},
		{"storage failure", journal.PhaseAborted, errors.Join(held, busy, errors.New("disk full")), false},
		{"unexpected cleanup", journal.PhaseAborted, errors.Join(held, fmt.Errorf("cleanup: %w: %w", worktree.ErrCleanupDeferred, errors.New("permission denied"))), false},
		{"no error", journal.PhaseAborted, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := deferredFamilyCancellation(test.phase, test.err); got != test.want {
				t.Fatalf("pending = %t, want %t", got, test.want)
			}
		})
	}
}
