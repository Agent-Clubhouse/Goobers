package decisiongate

import (
	"context"
	"hash/fnv"
)

// ShadowRecord pairs the model's view with what actually happened, so the two
// can be compared offline. It carries digests and numbers, never the reply.
type ShadowRecord struct {
	StateDigest string
	Verdict     ClaimVerdict
	Probability float64
	Confidence  float64
	Cached      bool
	// AgentClaimedBad is what the agent actually did, as detected by the
	// caller (for example a refusal pattern). InputValid is the deterministic
	// check result. InputValid && AgentClaimedBad is a ground-truth spurious
	// claim that needs no human label.
	AgentClaimedBad bool
	InputValid      bool
	Err             error
}

// Sampled reports whether key falls inside the sample fraction, stably, so the
// same run is always in or out. fraction <= 0 or >= 1 means everything.
func Sampled(key string, fraction float64) bool {
	if fraction <= 0 || fraction >= 1 {
		return true
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return float64(h.Sum32()%10000)/10000 < fraction
}

// Shadow scores a reply without influencing anything. Errors are recorded and
// swallowed: a shadow failure must never change a run.
func (g *Gate) Shadow(ctx context.Context, inputValid, agentClaimedBad bool, reply string) ShadowRecord {
	v, o, err := g.EvaluateClaim(ctx, inputValid, reply)
	return ShadowRecord{
		Verdict: v, Probability: o.Probability, Confidence: o.Confidence,
		Cached: o.Cached, AgentClaimedBad: agentClaimedBad, InputValid: inputValid, Err: err,
	}
}

// Tally summarizes shadow records. Spurious ground truth is InputValid &&
// AgentClaimedBad.
type Tally struct {
	Total, GroundSpurious, CaughtSpurious, MissedSpurious, FalseAlarms, Unsure, Errors int
}

// Add folds one record in.
func (t *Tally) Add(r ShadowRecord) {
	t.Total++
	if r.Err != nil {
		t.Errors++
	}
	if r.Verdict == ClaimUnsure {
		t.Unsure++
	}
	truth := r.InputValid && r.AgentClaimedBad
	flagged := r.Verdict == ClaimSpurious
	switch {
	case truth:
		t.GroundSpurious++
		if flagged {
			t.CaughtSpurious++
		} else {
			t.MissedSpurious++
		}
	case flagged:
		t.FalseAlarms++
	}
}
