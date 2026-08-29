package main

import (
	"reflect"
	"strings"
	"testing"
)

// The dispatch queues bind BESIDE the operator's own queues: order preserved,
// duplicates not re-added, so a queue named both ways is served by exactly
// one worker in this process.
func TestMergeQueues(t *testing.T) {
	got := mergeQueues(
		[]string{"goobers-engine", "goobers-dispatch.web.win-ci"},
		[]string{"goobers-dispatch.web.win-ci", "goobers-dispatch.web.linux-xl"},
	)
	want := []string{"goobers-engine", "goobers-dispatch.web.win-ci", "goobers-dispatch.web.linux-xl"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("mergeQueues = %v, want %v", got, want)
	}
}

// The dispatch wiring fails closed before any cluster contact: no surrender
// plane, or no loadable instance, refuses with the cause named.
func TestBuildStageDispatchFailsClosed(t *testing.T) {
	t.Run("missing instance", func(t *testing.T) {
		_, err := buildStageDispatch(t.TempDir(), "gaggle-web", "", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "instance config") {
			t.Fatalf("error = %v, want the instance-load refusal", err)
		}
	})
	t.Run("missing surrender plane", func(t *testing.T) {
		_, err := buildStageDispatch(t.TempDir(), "gaggle-web", "", "")
		if err == nil || !strings.Contains(err.Error(), "surrender plane") {
			t.Fatalf("error = %v, want the surrender-plane requirement named", err)
		}
	})
}
