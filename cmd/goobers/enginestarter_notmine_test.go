package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"

	"github.com/goobers/goobers/internal/engine"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

type reviewNotMineStarter struct {
	directory string
	fenceErr  error
	t         *testing.T
}

func (s reviewNotMineStarter) Start(_ context.Context, in engine.RunInput) (engine.StartResult, error) {
	pending, err := engine.PendingStartIntents(s.directory)
	if err != nil || len(pending) != 1 {
		s.t.Fatalf("missing provisional custody during start: %+v, %v", pending, err)
	}
	return engine.StartResult{RunID: in.RunID, AlreadyRunning: true, NotMine: true}, s.fenceErr
}
func TestReviewNotMineSuppressesCallerSideEffects(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(fmt.Sprint("stale=", stale), func(t *testing.T) {
			var fenceErr error
			if stale {
				fenceErr = errors.New("stale lease")
			}
			assertNotMineSuppressesEffects(t, fenceErr)
		})
	}
}

func assertNotMineSuppressesEffects(t *testing.T, fenceErr error) {
	t.Helper()
	temporal := &fakeEngineWorkflows{status: enumspb.WORKFLOW_EXECUTION_STATUS_COMPLETED, result: engine.RunResult{Status: engine.StatusCompleted, FinalState: "implement", NoWork: true}}
	f := newEngineStarterFixture(t, temporal, &recordingEngineStarter{})
	f.starter.runtime.Attach(reviewNotMineStarter{directory: filepath.Join(f.layout.SchedulerDir(), "engine-starts"), t: t, fenceErr: fenceErr}, &engineRunGuards{client: temporal}, f.live, time.Now)
	result, err := f.starter.Start(t.Context(), engineStartRequest("review-notmine"))
	if !errors.Is(err, engine.ErrStartNotMine) {
		t.Fatalf("NotMine error = %v", err)
	}
	pending, readErr := os.ReadDir(filepath.Join(f.layout.SchedulerDir(), "engine-starts"))
	if readErr != nil || len(pending) != 0 {
		t.Fatalf("provisional reservation not released: %v, %v", pending, readErr)
	}
	events, logErr := journal.ReadInstanceLog(f.logDir)
	if logErr != nil || len(events) != 0 {
		t.Fatalf("NotMine authored instance events: %+v, %v", events, logErr)
	}
	_, awaited, _ := temporal.snapshot()
	t.Logf("result=%+v err=%v terminal hooks=%v awaited=%v", result, err, f.hooks.order, awaited)
	if len(f.hooks.order) > 0 || len(awaited) > 0 {
		t.Error("NotMine caller attached to someone else's execution and ran terminal hooks")
	}
	if rd, e := journal.OpenRead(filepath.Join(f.layout.RunsDir(), "review-notmine")); e == nil {
		_ = rd
		t.Error("NotMine caller wrote run journal reservation")
	}
}

func TestProvisionalStartOwnershipAppearsInStatusAndDoctor(t *testing.T) {
	root := initDemo(t)
	layout := instance.NewLayout(root)
	_, err := engine.ProvisionStart(filepath.Join(layout.SchedulerDir(), "engine-starts"), engine.RunInput{RunID: "uncertain-run", Gaggle: "web", WorkflowName: "flow"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status := inspectStatusRoot(layout, time.Now())
	if len(status.StartOwnershipUncertain) != 1 || !strings.Contains(status.StartOwnershipUncertain[0], "start ownership uncertain") {
		t.Fatalf("status = %+v", status)
	}
	var stdout, stderr bytes.Buffer
	if code := runDoctor([]string{"--temporal-codec", "--report", "json", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("doctor=%d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "start ownership uncertain: uncertain-run") {
		t.Fatalf("doctor omitted quarantine: %s", stdout.String())
	}
}
