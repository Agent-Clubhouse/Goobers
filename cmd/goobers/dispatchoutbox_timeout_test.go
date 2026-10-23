package main

import (
	"context"
	"io"
	"os"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
)

func TestTimedOutCommandRemainsOutboxEligible(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv(dispatcher.EnvStageCommand, `["sh","-c","printf diagnostic > report.txt; sleep 5"]`)
	t.Setenv(dispatcher.EnvStageScript, "")
	t.Setenv(dispatcher.EnvStageTimeout, "50ms")
	t.Setenv(dispatcher.EnvStageOutbox, `["report.txt"]`)
	out := runStage(context.Background(), io.Discard, io.Discard)
	if out.Result.Status != apiv1.ResultFailure || out.Result.Error == nil ||
		out.Result.Error.Code != "stage_timeout" || !out.OutboxEligible {
		t.Fatalf("outcome=%+v", out)
	}
	if _, err := os.Stat("report.txt"); err != nil {
		t.Fatal(err)
	}
}
