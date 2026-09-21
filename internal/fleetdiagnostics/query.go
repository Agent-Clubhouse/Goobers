package fleetdiagnostics

import (
	"errors"
	"time"
)

// Usage distinguishes configured-but-unused, observed use, and unknown gaps.
type Usage struct {
	FeatureID   string    `json:"featureId"`
	Configured  *bool     `json:"configured"`
	State       string    `json:"state"`
	Count       *int64    `json:"count,omitempty"`
	Coverage    string    `json:"coverage"`
	BootID      string    `json:"bootId"`
	WindowStart time.Time `json:"windowStart"`
	WindowEnd   time.Time `json:"windowEnd"`
}

// Report is scoped to exactly one authenticated tenant. OwnerRoute is a routing
// hint for the company's tools, never an automatic message or inferred owner.
type Report struct {
	DiagnosticsDroppedRecords *int64         `json:"diagnosticsDroppedRecords,omitempty"`
	RequiredMCP               *MCPHealth     `json:"requiredMcp,omitempty"`
	Worker                    *WorkerHealth  `json:"worker,omitempty"`
	Backlog                   *BacklogHealth `json:"backlog,omitempty"`
	Identity
	State                 string       `json:"state"`
	Reason                string       `json:"reason"`
	Liveness              string       `json:"liveness"`
	ReceivedAt            time.Time    `json:"receivedAt"`
	ObservationAgeSeconds *float64     `json:"observationAgeSeconds,omitempty"`
	Coverage              string       `json:"coverage"`
	OwnerRoute            string       `json:"ownerRoute,omitempty"`
	Freshness             Freshness    `json:"freshness"`
	Features              []Usage      `json:"features"`
	Transitions           []Transition `json:"transitions"`
}

// Reports evaluates missing heartbeats against enrollment and the receiver's
// clock. Its query path also records bounded condition transitions, including
// entering and recovering from missing-heartbeat conditions.
func (b *Backend) Reports(tenant string) ([]Report, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	policy, ok := b.tenants[tenant]
	if !ok {
		return nil, errors.New("unauthorized tenant")
	}
	now := b.now()
	reports := make([]Report, 0, len(b.entries[tenant]))
	for _, key := range sortedKeys(b.entries[tenant]) {
		e := b.entries[tenant][key]
		r := evaluate(e, policy, now, b.maxClockSkew)
		if e.heartbeat != nil {
			r.Worker = workerReport(e.heartbeat.Worker, r.Liveness == "live")
		}
		r.Freshness = freshness(e.heartbeat, e.enrollment, policy.Catalogue, now)
		r.Features = featureReports(e, r.Liveness == "live", now, b.maxClockSkew)
		r.OwnerRoute = policy.Owners[r.OwnerRef]
		recordTransition(e, r, now)
		r.Transitions = append([]Transition(nil), e.transitions...)
		reports = append(reports, r)
	}
	return reports, nil
}
func evaluate(e *entry, policy Tenant, now time.Time, skew time.Duration) Report {
	r := evaluateHealth(e, policy, now, skew)
	if e.heartbeat != nil {
		r.RequiredMCP = mcpReport(e.heartbeat.RequiredMCP, r.Liveness == "live")
		r.Backlog = backlogReport(e.heartbeat.Backlog, r.Liveness == "live", now)
	}
	return r
}

func evaluateHealth(e *entry, policy Tenant, now time.Time, skew time.Duration) Report {
	r := Report{Identity: e.enrollment.Identity, State: "unknown", Reason: "awaiting_first_observation", Liveness: "unobserved", Coverage: "unknown", ReceivedAt: e.receivedAt}
	lastSeen := e.enrolledAt
	if h := e.heartbeat; h != nil {
		r.Identity = h.Identity
		if r.OwnerRef == "" {
			r.OwnerRef = e.enrollment.OwnerRef
		}
		if h.DiagnosticsDroppedRecords != nil {
			count := *h.DiagnosticsDroppedRecords
			r.DiagnosticsDroppedRecords = &count
		}
		r.Coverage = h.Coverage
		r.State = h.State
		r.Reason = h.ReasonCode
		lastSeen = e.receivedAt
		age := now.Sub(h.ObservedAt).Seconds()
		r.ObservationAgeSeconds = &age
	}
	if policy.CollectorUnavailable {
		r.State = "unknown"
		r.Reason = "collector_unavailable"
		return r
	}
	boundary := e.enrollment.HeartbeatInterval * time.Duration(e.enrollment.MissedIntervals)
	if now.Sub(lastSeen) >= boundary {
		r.State = "unknown"
		r.Reason = "missing_heartbeat"
		r.Liveness = "unreachable"
		return r
	}
	if e.heartbeat == nil {
		return r
	}
	if e.heartbeat.ObservedAt.After(now.Add(skew)) || now.Before(lastSeen) {
		r.State = "unknown"
		r.Reason = "clock_skew"
		return r
	}
	if now.Sub(e.heartbeat.ObservedAt) >= boundary+skew {
		r.State = "unknown"
		r.Reason = "stale_observation"
		return r
	}
	r.Liveness = "live"
	// Partial evidence may carry a positive observation, but cannot prove idle
	// or a definitive stalled condition from missing/zero counters.
	if r.Coverage != "complete" && (r.State == "idle" || r.State == "stalled") {
		r.State = "unknown"
		r.Reason = "observation_incomplete"
	}
	return r
}
func featureReports(e *entry, live bool, now time.Time, skew time.Duration) []Usage {
	result := make([]Usage, 0, len(FeatureIDs()))
	for _, id := range FeatureIDs() {
		f, ok := e.features[id]
		if !ok {
			result = append(result, Usage{FeatureID: id, State: "unknown", Coverage: "unknown"})
			continue
		}
		u := Usage{FeatureID: id, Configured: &f.Configured, State: "unknown", Coverage: f.Coverage, BootID: f.BootID, WindowStart: f.WindowStart, WindowEnd: f.WindowEnd}
		if live && !f.ObservedAt.After(now) && now.Sub(f.ObservedAt) < e.enrollment.HeartbeatInterval*time.Duration(e.enrollment.MissedIntervals)+skew && f.Count != nil {
			if *f.Count > 0 {
				count := *f.Count
				u.Count = &count
				u.State = "used"
			} else if f.Coverage == "complete" {
				count := int64(0)
				u.Count = &count
				u.State = "unused"
			}
		}
		result = append(result, u)
	}
	return result
}

func recordTransition(e *entry, r Report, now time.Time) {
	recordMCPTransition(e, r.RequiredMCP, now)
	state := r.Liveness + ":" + r.State + ":" + r.Reason
	if state == e.lastState {
		return
	}
	e.transitions = append(e.transitions, Transition{At: now, From: e.lastState, To: r.State, Reason: r.Reason})
	if len(e.transitions) > MaxTransitions {
		e.transitions = append([]Transition(nil), e.transitions[len(e.transitions)-MaxTransitions:]...)
	}
	e.lastState = state
}
