package journal

import "testing"

func TestAgentPodOrdinalRejectsUnrecognizedKeys(t *testing.T) {
	for _, key := range []any{nil, 3, "", "work/agent.lifecycle/1", "pod/0/work/op", "pod/-1/work/op", "pod/+1/work/op", "pod/01/work/op", "pod/1/", "pod/1", "pod/18446744073709551616/work/op"} {
		if got := agentPodOrdinal(Event{Runner: map[string]any{"emitKey": key}}); got != 0 {
			t.Errorf("unrecognized key %v has authority %d", key, got)
		}
	}
}
