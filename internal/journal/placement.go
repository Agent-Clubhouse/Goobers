package journal

import "time"

// PlacementRunnerSelf is the runner name placement provenance records when a
// stage attempt executes on the daemon's own host (modes 1–2): the implicit
// "self" entry of the runners inventory. It mirrors
// internal/instance.RunnerHostSelfName — the journal cannot import instance
// (instance is a config concern above the journal), so the value is pinned by
// TestPlacementSelfMatchesInventorySelfName in internal/runner.
const PlacementRunnerSelf = "self"

// Placement payload keys under Event.Runner. The runner.* map is the ONLY
// sanctioned runner-specific divergence (event.go), so the payload rides it as
// scalars rather than new Event fields; timestamps are RFC3339Nano strings so
// the payload round-trips JSON identically to how it was written.
const (
	placementKeyRunner       = "runner"
	placementKeyNode         = "node"
	placementKeyOS           = "os"
	placementKeyImage        = "image"
	placementKeyPod          = "pod"
	placementKeyQueuedAt     = "queuedAt"
	placementKeyPodStartedAt = "podStartedAt"
)

// Placement is the typed payload of a runner.placement event
// (goobernetes-architecture.md §7): where one stage attempt physically
// executed, as far as the EXECUTING substrate knows. Every field except
// Runner is optional — a local run knows no pod and waits in no queue, and
// recording only what is known is the contract that lets the mode-3
// dispatcher (#3513) fill node/pod/queue-wait through this same shape rather
// than adding a second mechanism.
//
// The JSON tags are the read-model wire shape: readservice.StageAttempt
// serves *Placement verbatim on the HTTP contract, so absent fields are
// absent there too.
type Placement struct {
	// Runner is the resolved runners-inventory entry name;
	// PlacementRunnerSelf for the daemon's own host.
	Runner string `json:"runner"`
	// Node is the cluster node (or, for self placement, the host) the
	// attempt executed on, when known.
	Node string `json:"node,omitempty"`
	// OS is the GOOS of the executing substrate.
	OS string `json:"os,omitempty"`
	// Image is the container image reference the attempt ran under, when the
	// substrate is containerized and knows it.
	Image string `json:"image,omitempty"`
	// Pod is the pod identity for containerized attempts (empty on bare
	// hosts). Distinct pods per attempt is the smoke's fresh-pod observer
	// (goobernetes-architecture.md §11 item 6).
	Pod string `json:"pod,omitempty"`
	// QueuedAt is when the attempt entered the dispatch fabric, and
	// PodStartedAt when its pod began executing — together the
	// dispatch-latency carriers the scale rung reads
	// (goobernetes-smoke.md §6.3). Both nil for attempts that never queued
	// (modes 1–2).
	QueuedAt     *time.Time `json:"queuedAt,omitempty"`
	PodStartedAt *time.Time `json:"podStartedAt,omitempty"`
}

// PlacementEvent builds the runner.placement journal event for one stage
// attempt. Stage/Attempt/AttemptClass mirror the attempt's stage.started
// identity so read-model projections correlate the provenance to the right
// attempt; the payload lives entirely under Runner and is excluded from
// conformance (D14).
func PlacementEvent(stage string, attempt int, class AttemptClass, p Placement) Event {
	payload := map[string]any{placementKeyRunner: p.Runner}
	setNonEmpty := func(key, value string) {
		if value != "" {
			payload[key] = value
		}
	}
	setNonEmpty(placementKeyNode, p.Node)
	setNonEmpty(placementKeyOS, p.OS)
	setNonEmpty(placementKeyImage, p.Image)
	setNonEmpty(placementKeyPod, p.Pod)
	if p.QueuedAt != nil {
		payload[placementKeyQueuedAt] = p.QueuedAt.Format(time.RFC3339Nano)
	}
	if p.PodStartedAt != nil {
		payload[placementKeyPodStartedAt] = p.PodStartedAt.Format(time.RFC3339Nano)
	}
	return Event{
		Type:         EventRunnerPlacement,
		Stage:        stage,
		Attempt:      attempt,
		AttemptClass: class,
		Runner:       payload,
	}
}

// PlacementFromEvent decodes a runner.placement event's payload. It reports
// false for any other event type and for a payload missing the one required
// field (the runner name) — the lenient posture every journal reader takes
// toward runner.* content it does not understand.
func PlacementFromEvent(e Event) (Placement, bool) {
	if e.Type != EventRunnerPlacement || e.Runner == nil {
		return Placement{}, false
	}
	str := func(key string) string {
		value, _ := e.Runner[key].(string)
		return value
	}
	p := Placement{
		Runner:       str(placementKeyRunner),
		Node:         str(placementKeyNode),
		OS:           str(placementKeyOS),
		Image:        str(placementKeyImage),
		Pod:          str(placementKeyPod),
		QueuedAt:     placementTime(e.Runner[placementKeyQueuedAt]),
		PodStartedAt: placementTime(e.Runner[placementKeyPodStartedAt]),
	}
	if p.Runner == "" {
		return Placement{}, false
	}
	return p, true
}

// placementTime reads one payload timestamp: an RFC3339Nano string after a
// JSON round trip, or a time.Time on an event still in memory. Unparseable
// values decode as absent rather than failing the whole payload.
func placementTime(value any) *time.Time {
	switch v := value.(type) {
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return &parsed
		}
	case time.Time:
		return &v
	}
	return nil
}
