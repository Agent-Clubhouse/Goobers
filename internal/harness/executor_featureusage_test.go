package harness

import (
	"context"
	"errors"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/diagnostics/featureusage"
)

func TestAdapterInvocationRecordsActualFiniteFeature(t *testing.T) {
	for _, name := range []string{"copilot-cli", "claude-code", "codex", "private-custom-adapter"} {
		t.Run(name, func(t *testing.T) {
			called := false
			failure := errors.New("adapter failed after invocation")
			adapter := &FakeAdapter{AdapterName: name, Act: func(context.Context, RunRequest) error { called = true; return failure }}
			rec := &fakeRecorder{}
			e := &Executor{adapter: adapter, recorder: rec}
			_, err := e.runAdapter(context.Background(), RunRequest{Envelope: apiv1.InvocationEnvelope{TaskID: "work"}}, nil)
			if !called || !errors.Is(err, failure) {
				t.Fatalf("called=%v err=%v", called, err)
			}
			if featureusage.AdapterID(name) == "" {
				if len(rec.events) != 0 {
					t.Fatal("unknown feature leaked")
				}
				return
			}
			if len(rec.events) != 1 || rec.events[0].Runner["featureId"] != featureusage.AdapterID(name) || rec.events[0].Runner["count"] != int64(1) {
				t.Fatal(rec.events)
			}
			rec.err = errors.New("diagnostic journal unavailable")
			_, err = e.runAdapter(context.Background(), RunRequest{}, nil)
			if !errors.Is(err, failure) {
				t.Fatal("diagnostics replaced invocation failure", err)
			}
		})
	}
}
