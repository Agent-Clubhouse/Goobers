package daemonheartbeat

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestUpdatePendingClause(t *testing.T) {
	var pending PendingUpdate
	if got := pending.Clause(); got != "" {
		t.Errorf("zero-value clause() = %q, want empty", got)
	}
	pending.Set("v1.2.3")
	if got := pending.Clause(); got != "; update v1.2.3 available" {
		t.Errorf("clause() = %q", got)
	}
	pending.Set("")
	if got := pending.Clause(); got != "" {
		t.Errorf("cleared clause() = %q, want empty", got)
	}
	// A nil holder is valid for a heartbeat with no checker wired up.
	if got := (*PendingUpdate)(nil).Clause(); got != "" {
		t.Errorf("(*PendingUpdate)(nil).Clause() = %q, want empty", got)
	}
}

// The point of the clause is that it survives in a stream the one-shot notice
// scrolls out of, so assert it reaches the rendered heartbeat line itself —
// and that a current build leaves that line byte-identical to before.
func TestHeartbeatCarriesUpdateClause(t *testing.T) {
	tests := []struct {
		name    string
		pending *PendingUpdate
		want    string
		absent  string
	}{
		{name: "update pending", pending: pendingAt("v0.5.0"), want: "; update v0.5.0 available"},
		{name: "build current", pending: &PendingUpdate{}, absent: "update"},
		{name: "no checker wired", pending: nil, absent: "update"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			log, _, err := journal.OpenInstanceLog(dir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = log.Close() })
			tail, err := journal.OpenInstanceLogTail(dir)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			stdout := newDaemonOutput()
			done := make(chan struct{})
			go Emit(ctx, stdout, dir, func() int { return 1 }, tail, nil, 10*time.Millisecond, test.pending, done)
			select {
			case <-stdout.heartbeat:
			case <-time.After(10 * time.Second):
				t.Error("heartbeat was not emitted")
			}
			cancel()
			<-done

			output := stdout.String()
			if test.want != "" && !strings.Contains(output, test.want) {
				t.Errorf("heartbeat = %q, want it to contain %q", output, test.want)
			}
			if test.absent != "" && strings.Contains(output, test.absent) {
				t.Errorf("heartbeat = %q, want no %q clause", output, test.absent)
			}
		})
	}
}

func pendingAt(version string) *PendingUpdate {
	pending := &PendingUpdate{}
	pending.Set(version)
	return pending
}
