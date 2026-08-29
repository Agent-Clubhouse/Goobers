package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"
)

// ProbeOutcome classifies one egress probe. The three-way split is the point:
// a NetworkPolicy denial is a silent DROP (the connection TIMES OUT — curl
// exit 28), while a reachable host that refuses the port answers RST
// (REFUSED) and a healthy service ACCEPTS. Collapsing these into
// reachable/unreachable is how a down host impersonates a policy denial.
type ProbeOutcome int

// Probe outcomes.
const (
	// ProbeUnknown is the zero value — no probe ran.
	ProbeUnknown ProbeOutcome = iota
	// ProbeReachable means the connection was accepted.
	ProbeReachable
	// ProbeRefused means the host answered with a refusal (RST) — the host
	// is UP; nothing silently dropped the traffic.
	ProbeRefused
	// ProbeTimedOut means the connection attempt timed out — the signature
	// of a deny-first policy DROP (and also of a down host, which is exactly
	// why a bare ProbeTimedOut proves nothing on its own).
	ProbeTimedOut
)

// String names the outcome for diagnostics.
func (o ProbeOutcome) String() string {
	switch o {
	case ProbeReachable:
		return "reachable"
	case ProbeRefused:
		return "refused"
	case ProbeTimedOut:
		return "timed-out"
	default:
		return "unknown"
	}
}

// EgressTriple is the §8-item-5 acceptance shape: proving "the dispatcher's
// egress reaches only the allowed set" takes THREE observations, never a bare
// denial — a bare denial is also what a down host or a partition produces.
type EgressTriple struct {
	// DeniedHostFromHere is the probe of a non-allowed host FROM the
	// component under test — must be ProbeTimedOut (the policy DROP).
	DeniedHostFromHere ProbeOutcome
	// DeniedHostFromControl is the probe of the SAME host from a vantage
	// point outside the policy — must be ProbeReachable, proving the host is
	// up and the denial is the policy's, not an outage.
	DeniedHostFromControl ProbeOutcome
	// AllowedHostFromHere is the probe of an allowed-set host from the
	// component under test — must be ProbeReachable, the positive control
	// proving the component's network path works at all.
	AllowedHostFromHere ProbeOutcome
}

// Verdict evaluates the triple. Confirmed is returned ONLY when all three
// legs hold; every other combination returns unproven with a diagnostic
// naming which leg failed and what that ambiguity means.
func (t EgressTriple) Verdict() (bool, string) {
	if t.AllowedHostFromHere != ProbeReachable {
		return false, fmt.Sprintf(
			"positive control failed: the allowed-set host probed %s from the component — its network path may be broken entirely, so a denial elsewhere proves nothing",
			t.AllowedHostFromHere)
	}
	if t.DeniedHostFromControl != ProbeReachable {
		return false, fmt.Sprintf(
			"control leg failed: the denied host probed %s from OUTSIDE the policy — it may simply be down, and a down host produces the same bare denial a policy does",
			t.DeniedHostFromControl)
	}
	if t.DeniedHostFromHere != ProbeTimedOut {
		return false, fmt.Sprintf(
			"denial leg failed: the non-allowed host probed %s from the component — a deny-first policy DROP times out; anything else means the policy did not drop the traffic",
			t.DeniedHostFromHere)
	}
	return true, "egress posture confirmed: non-allowed host dropped from the component, same host up from the control vantage, allowed host reachable from the component"
}

// ProbeTCP dials addr once with the given timeout and classifies the result.
// It is the probe primitive the doctor/e2e collateral drives from inside the
// dispatcher and from a control vantage to assemble an EgressTriple.
func ProbeTCP(ctx context.Context, addr string, timeout time.Duration) ProbeOutcome {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err == nil {
		_ = conn.Close()
		return ProbeReachable
	}
	var netErr net.Error
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout()) {
		return ProbeTimedOut
	}
	// syscall.ECONNREFUSED matches on Unix; Windows surfaces
	// WSAECONNREFUSED, which errors.Is does not unify with it, so the
	// message fallback keeps the classifier honest cross-platform (this is
	// a diagnostic classifier, not control flow).
	if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(strings.ToLower(err.Error()), "refused") {
		return ProbeRefused
	}
	return ProbeUnknown
}
