package dispatcher

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

// §8 item 5 semantics as code: the egress proof is the EXIT-CODE TRIPLE.
// Confirmed requires ALL THREE legs — the non-allowed host DROPPED from the
// component, the SAME host reachable from a control vantage (it is up), and
// an allowed host reachable from the component (positive control). A bare
// denial NEVER confirms, because a down host produces a bare denial too.
func TestEgressTripleVerdict(t *testing.T) {
	confirmed, _ := EgressTriple{
		DeniedHostFromHere:    ProbeTimedOut,
		DeniedHostFromControl: ProbeReachable,
		AllowedHostFromHere:   ProbeReachable,
	}.Verdict()
	if !confirmed {
		t.Fatal("the complete triple must confirm the posture")
	}

	// THE trap this shape exists to close: a bare denial with the control
	// host down is UNPROVEN, and the diagnostic names the ambiguity.
	bare := EgressTriple{
		DeniedHostFromHere:    ProbeTimedOut,
		DeniedHostFromControl: ProbeTimedOut, // host may simply be down
		AllowedHostFromHere:   ProbeReachable,
	}
	confirmed, why := bare.Verdict()
	if confirmed {
		t.Fatal("a bare denial (control host unreachable) must NOT confirm — a down host produces the same denial")
	}
	if !strings.Contains(why, "down") {
		t.Errorf("diagnostic %q does not name the down-host ambiguity", why)
	}

	// Broken positive control: the component may have no network at all.
	confirmed, why = EgressTriple{
		DeniedHostFromHere:    ProbeTimedOut,
		DeniedHostFromControl: ProbeReachable,
		AllowedHostFromHere:   ProbeTimedOut,
	}.Verdict()
	if confirmed {
		t.Fatal("a dead positive control must not confirm")
	}
	if !strings.Contains(why, "positive control") {
		t.Errorf("diagnostic %q does not name the positive control", why)
	}

	// A REFUSED denial leg means the traffic was answered, not dropped: the
	// policy did not deny it.
	confirmed, why = EgressTriple{
		DeniedHostFromHere:    ProbeRefused,
		DeniedHostFromControl: ProbeReachable,
		AllowedHostFromHere:   ProbeReachable,
	}.Verdict()
	if confirmed {
		t.Fatal("an answered (refused) probe is not a policy drop")
	}
	if !strings.Contains(why, "drop") {
		t.Errorf("diagnostic %q does not explain the drop requirement", why)
	}
}

// ProbeTCP classifies real socket outcomes: an accepting listener is
// reachable; a closed local port is refused (the host answered — up, not
// denied). Both probes stay on loopback.
func TestProbeTCPClassification(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := listener.Addr().String()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() { _ = listener.Close() })

	if outcome := ProbeTCP(context.Background(), addr, time.Second); outcome != ProbeReachable {
		t.Fatalf("accepting listener probed %s, want reachable", outcome)
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()
	if outcome := ProbeTCP(context.Background(), closedAddr, time.Second); outcome != ProbeRefused {
		t.Fatalf("closed port probed %s, want refused", outcome)
	}
}
