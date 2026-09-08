package journal

import (
	"strconv"
	"strings"
)

type agentStageIdentity struct{ run, stage string }

// latestPodAgentEvents scopes agent projections to the newest physical pod for
// each run/stage. The pod writer's persisted Runner.emitKey contract is
// "pod/<positive decimal dispatch ordinal>/<operation key>"; ordinals increase
// across retries and graph revisits even when logical Attempt resets to one.
// This origin identity, unlike journal Seq or pod timestamps, remains valid
// when an older pod's event arrives late. Non-agent events are left intact.
//
// A stage without recognized pod keys keeps legacy projection behavior. Legacy
// events cannot unambiguously identify revisits; guessing from arrival order or
// clocks would let an old invocation replace current work. Once stamped events
// exist for a stage, unbound legacy events cannot supersede them.
func latestPodAgentEvents(events []Event) []Event {
	latest := make(map[agentStageIdentity]uint64)
	for _, event := range events {
		if event.Type != EventAgentLifecycle || event.Agent == nil {
			continue
		}
		stage := agentStageIdentity{event.Agent.RunID, event.Agent.Stage}
		if ordinal := agentPodOrdinal(event); ordinal > latest[stage] {
			latest[stage] = ordinal
		}
	}
	filtered := make([]Event, 0, len(events))
	for _, event := range events {
		if event.Type == EventAgentLifecycle && event.Agent != nil {
			stage := agentStageIdentity{event.Agent.RunID, event.Agent.Stage}
			if ordinal := latest[stage]; ordinal > 0 && agentPodOrdinal(event) != ordinal {
				continue
			}
		}
		filtered = append(filtered, event)
	}
	return filtered
}

// agentPodOrdinal recognizes only the pod writer's complete positive-ordinal
// prefix. Other emit-key formats, malformed values and unstamped journals have
// no physical ordering authority. This parser is shared by tree and usage
// projections; it does not alter normative event lineage or charging policy.
func agentPodOrdinal(event Event) uint64 {
	key, _ := event.Runner["emitKey"].(string)
	rest, ok := strings.CutPrefix(key, "pod/")
	if !ok {
		return 0
	}
	ordinal, operation, ok := strings.Cut(rest, "/")
	if !ok || operation == "" {
		return 0
	}
	n, err := strconv.ParseUint(ordinal, 10, 64)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != ordinal {
		return 0
	}
	return n
}
