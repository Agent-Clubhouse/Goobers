package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/goobers/goobers/internal/journal"
)

func TestExecutorErrorEventPreservesWrappedCauses(t *testing.T) {
	err := fmt.Errorf("dispatch stage: %w", fmt.Errorf("prepare workspace: %w", errors.New("workspace unavailable")))

	ev := executorErrorEvent("implement", 2, journal.AttemptPolicy, journal.AttemptInfra, err, nil)

	if ev.Error == nil {
		t.Fatal("executorErrorEvent error detail is nil")
	}
	if ev.Error.Code != "executor_error" || ev.Error.Message != err.Error() {
		t.Fatalf("error detail = %+v, want executor_error and complete message %q", ev.Error, err.Error())
	}
	got := make([]string, len(ev.Error.Causes))
	for i, cause := range ev.Error.Causes {
		got[i] = cause.Message
	}
	want := []string{"dispatch stage", "prepare workspace", "workspace unavailable"}
	if len(got) != len(want) {
		t.Fatalf("causes = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("causes = %#v, want %#v", got, want)
		}
	}
}
