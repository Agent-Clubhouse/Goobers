package harness

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/diagnostics/executiondeadline"
)

func TestDeadlineProcessHelper(t *testing.T) {
	if len(os.Args) > 1 && os.Args[len(os.Args)-1] == "deadline-helper" {
		time.Sleep(10 * time.Millisecond)
	}
}
func TestProcessDeadlineUsesActualContextAndClears(t *testing.T) {
	recorder := &fakeRecorder{}
	deadline := time.Now().Add(10 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	ctx = executiondeadline.WithRecorder(ctx, recorder, "work", 3)
	_, err := (ExecProcessRunner{}).Run(ctx, ProcessRequest{Command: []string{os.Args[0], "-test.run=^TestDeadlineProcessHelper$", "--", "deadline-helper"}, Timeout: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(recorder.events) != 2 {
		t.Fatalf("boundary events=%+v", recorder.events)
	}
	begin, end := recorder.events[0], recorder.events[1]
	actual, err := time.Parse(time.RFC3339Nano, begin.Runner["deadline"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if !actual.Equal(deadline) || begin.Attempt != 3 || begin.Runner["executionState"] != "active" || end.Runner["executionState"] != "finished" || begin.Runner["executionId"] != end.Runner["executionId"] {
		t.Fatalf("actual bound not retained: %+v %+v", begin, end)
	}
}
