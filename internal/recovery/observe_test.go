package recovery

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestClassifyRestoreFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want RestoreFailureReason
	}{
		{"base missing", fmt.Errorf("wrap: %w", errRestoreBaseMissing), RestoreFailureReasonBaseMissing},
		{"archive invalid", fmt.Errorf("wrap: %w", errRestoreArchiveInvalid), RestoreFailureReasonArchiveInvalid},
		{"import failed", fmt.Errorf("wrap: %w", errRestoreImportFailed), RestoreFailureReasonImportFailed},
		{"unclassified defaults to import failed", errors.New("some other failure"), RestoreFailureReasonImportFailed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyRestoreFailure(c.err); got != c.want {
				t.Fatalf("classifyRestoreFailure(%v) = %q, want %q", c.err, got, c.want)
			}
		})
	}
}

func TestWithSnapshotObserverNilIsNoop(t *testing.T) {
	ctx := WithSnapshotObserver(context.Background(), nil)
	if observeSnapshot(ctx) != nil {
		t.Fatal("expected no observer attached for a nil observer")
	}
}
