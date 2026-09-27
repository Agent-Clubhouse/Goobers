package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/recovery"
	"github.com/goobers/goobers/internal/worktree"
)

// TestClassifyRecoveryCapacityGuardDistinguishesCaptureFromCapacity is
// #5352's guard-classification requirement: an inventory-full refusal, a git
// capture failure, and every other handoff refusal must remain
// distinguishable in the wrapped error chain, since #5264's
// classifyCleanupWarning reads that chain to pick an operator's remediation.
func TestClassifyRecoveryCapacityGuardDistinguishesCaptureFromCapacity(t *testing.T) {
	for _, tc := range []struct {
		name          string
		guardErr      error
		wantCapacity  bool
		wantCapture   bool
		wantUnwrapped bool
	}{
		{
			name:         "inventory full",
			guardErr:     fmt.Errorf("recovery cleanup: %w", recovery.ErrInventoryFull),
			wantCapacity: true,
		},
		{
			name: "capture failure: missing object",
			guardErr: fmt.Errorf("recovery handoff for run-1: capture recovery patch: %w",
				&recovery.CaptureError{Subcommand: "merge-base", ExitCode: 128, Class: recovery.CaptureErrorMissingObject}),
			wantCapture: true,
		},
		{
			name: "capture failure: locked",
			guardErr: fmt.Errorf("recovery handoff for run-1: capture recovery index: %w",
				&recovery.CaptureError{Subcommand: "add", ExitCode: 128, Class: recovery.CaptureErrorLocked}),
			wantCapture: true,
		},
		{
			name:          "unrelated handoff failure",
			guardErr:      errors.New("recovery cleanup requires verified repository and run ownership"),
			wantUnwrapped: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := classifyRecoveryCapacityGuard(func(context.Context, worktree.CleanupTarget) error {
				return tc.guardErr
			})
			err := guard(context.Background(), worktree.CleanupTarget{})
			if err == nil {
				t.Fatal("expected a wrapped error")
			}
			gotCapacity := errors.Is(err, worktree.ErrCleanupRecoveryCapacity)
			gotCapture := errors.Is(err, worktree.ErrCleanupRecoveryCapture)
			if gotCapacity != tc.wantCapacity {
				t.Errorf("errors.Is(ErrCleanupRecoveryCapacity) = %v, want %v (err: %v)", gotCapacity, tc.wantCapacity, err)
			}
			if gotCapture != tc.wantCapture {
				t.Errorf("errors.Is(ErrCleanupRecoveryCapture) = %v, want %v (err: %v)", gotCapture, tc.wantCapture, err)
			}
			if tc.wantCapacity && tc.wantCapture {
				t.Fatal("test case must not want both classes")
			}
			if tc.wantUnwrapped && !errors.Is(err, tc.guardErr) {
				t.Errorf("unrelated failure was wrapped: %v", err)
			}
			// The original evidence must still be reachable regardless of
			// classification: an operator reading only the top-level
			// sentinel would otherwise lose the subcommand/stderr/exit code.
			if tc.wantCapture {
				var capture *recovery.CaptureError
				if !errors.As(err, &capture) {
					t.Fatal("wrapped error lost the underlying *recovery.CaptureError")
				}
			}
		})
	}
}

// TestClassifyRecoveryCapacityGuardNilCallbackAndSuccess covers the
// pass-through cases classifyRecoveryCapacityGuard's callers rely on.
func TestClassifyRecoveryCapacityGuardNilCallbackAndSuccess(t *testing.T) {
	if classifyRecoveryCapacityGuard(nil) != nil {
		t.Error("a nil callback must produce a nil guard")
	}
	guard := classifyRecoveryCapacityGuard(func(context.Context, worktree.CleanupTarget) error {
		return nil
	})
	if err := guard(context.Background(), worktree.CleanupTarget{}); err != nil {
		t.Errorf("a succeeding callback must not be wrapped into an error: %v", err)
	}
}
