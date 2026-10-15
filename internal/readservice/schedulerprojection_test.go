package readservice

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSchedulerProjectorLogsRefreshFailureOnce(t *testing.T) {
	service, layout, _ := fixtureService(t)
	dir := layout.SchedulerDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	p := &schedulerStateProjector{
		service: service,
		logger:  slog.New(slog.NewTextHandler(&buf, nil)),
	}
	p.refresh(context.Background())
	p.refresh(context.Background())
	if got := strings.Count(buf.String(), "refresh failed"); got != 1 {
		t.Fatalf("failure logged %d times, want 1: %q", got, buf.String())
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	p.refresh(context.Background())
	if !strings.Contains(buf.String(), "recovered") {
		t.Fatalf("missing recovery log: %q", buf.String())
	}
}

func TestSchedulerProjectorCancelledRefreshStaysSilent(t *testing.T) {
	service, _, _ := fixtureService(t)
	var buf bytes.Buffer
	p := &schedulerStateProjector{
		service: service,
		logger:  slog.New(slog.NewTextHandler(&buf, nil)),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.recordRefreshFailure(ctx, ctx.Err())
	if buf.Len() != 0 {
		t.Fatalf("cancel logged: %q", buf.String())
	}
}
