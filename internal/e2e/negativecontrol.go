package e2e

import (
	"fmt"
	"net"
	"strings"
)

// NegativeControlObserver is S9's named observer (goobernetes-smoke.md §4
// S9): "an exit-code TRIPLE, machine-checkable (decisions 004/008), all
// three legs REQUIRED."
const NegativeControlObserver = "curl exit-code triple: denial (exit 28, never 6) + positive control (no shared prefix with any model endpoint) + second-runner-class reachability control"

// Curl exit codes S9 distinguishes. curl's own numbering, not this repo's
// invention — pinned here because the whole point of the criterion is that
// the SPECIFIC code is what's checked, never log prose (§4 S9: "never log
// prose").
const (
	// CurlExitCouldNotResolveHost is curl's DNS-failure exit code. S9
	// requires the denial NEVER present as this — a DNS failure proves
	// nothing about the NetworkPolicy.
	CurlExitCouldNotResolveHost = 6
	// CurlExitOperationTimeout is curl's exit code for a connection that
	// timed out against a resolved address — the deny-first NetworkPolicy
	// DROP signature S9 requires for a real denial.
	CurlExitOperationTimeout = 28
	// CurlExitSuccess is curl's success exit code, required for both the
	// positive control and the second-runner-class control.
	CurlExitSuccess = 0
)

// This package's negative-control model is deliberately NOT
// internal/dispatcher.EgressTriple (internal/dispatcher/probe.go), even
// though both encode a three-leg proof against the same false-green risk:
// EgressTriple classifies a raw TCP dial (internal/dispatcher.ProbeTCP) for
// the DISPATCHER's own egress (goobernetes-architecture.md §8 item 5).
// S9 is a different acceptance item — a restricted RUNNER's `network:
// allowlist` stage — with different, curl-specific semantics EgressTriple's
// ProbeOutcome cannot express (the exit-28-never-6 distinction, and the
// "shares no prefix with any model endpoint" requirement has no TCP-dial
// analogue). The three-leg SHAPE is intentionally the same discipline,
// restated for curl.

// NegativeControlProbe is one curl invocation's outcome: its exit code and
// the endpoint it targeted. No log text — S9 requires the classification be
// exit-code-only ("never log prose").
type NegativeControlProbe struct {
	Endpoint string
	ExitCode int
}

// NegativeControlTriple is S9's three required legs (goobernetes-smoke.md
// §4 S9, "all three legs REQUIRED"):
type NegativeControlTriple struct {
	// Denial is the restricted runner probing a non-allowlisted endpoint.
	Denial NegativeControlProbe
	// PositiveControl is the SAME restricted runner probing an allowlisted
	// endpoint that shares no prefix with any model endpoint.
	PositiveControl NegativeControlProbe
	// ModelEndpoints is the set of model endpoints PositiveControl must
	// share no prefix with (the false-green a shared-prefix control would
	// produce, per §4 S9).
	ModelEndpoints []string
	// ControlVantage is a SECOND runner-class pod probing Denial.Endpoint —
	// proving denial is attributable to the restricted class, not to broken
	// networking generally.
	ControlVantage NegativeControlProbe
}

// hostOnly strips a trailing ":<port>" from endpoint (net.SplitHostPort
// handles bracketed IPv6 too) so prefix comparison is over the HOST, not an
// incidental port suffix that would otherwise defeat the comparison
// entirely (two endpoints on the same host but different ports would
// compare unequal end-to-end even though the host itself is identical).
// Endpoints with no port pass through unchanged.
func hostOnly(endpoint string) string {
	if host, _, err := net.SplitHostPort(endpoint); err == nil {
		return host
	}
	return endpoint
}

// sharesPrefix reports whether a and b share a leading run of
// dot-separated labels — the shape a wildcard-allowlist bypass would
// produce (an allowlisted "api.example.com" masking a denial test against
// "api.example.com.evil.example" or vice versa). Case-insensitive, port
// stripped, either direction counts.
func sharesPrefix(a, b string) bool {
	a = strings.ToLower(strings.TrimSuffix(hostOnly(a), "."))
	b = strings.ToLower(strings.TrimSuffix(hostOnly(b), "."))
	if a == "" || b == "" {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// AssertNegativeControlTriple is S9: the restricted Linux runner's
// network:allowlist restriction is proven by all three legs holding at
// once.
func AssertNegativeControlTriple(triple NegativeControlTriple) AssertionResult {
	if triple.Denial.Endpoint == "" || triple.PositiveControl.Endpoint == "" || triple.ControlVantage.Endpoint == "" {
		return invalid("triple is missing an endpoint for one or more legs — the probes never ran", triple)
	}

	// Leg 1: denial. Exit 6 (DNS) is an explicit, named non-proof — checked
	// FIRST and distinctly from "any non-28 code", since a DNS failure is
	// the specific false-green §4 S9 calls out by name.
	if triple.Denial.ExitCode == CurlExitCouldNotResolveHost {
		return classify("", false,
			fmt.Sprintf("denial leg exited %d (DNS failure) against %q — this is explicitly not proof of policy denial (goobernetes-smoke.md §4 S9)", CurlExitCouldNotResolveHost, triple.Denial.Endpoint),
			nil, triple)
	}
	if triple.Denial.ExitCode != CurlExitOperationTimeout {
		return classify("", false,
			fmt.Sprintf("denial leg exited %d against %q, want %d (connection timed out against a resolved address)", triple.Denial.ExitCode, triple.Denial.Endpoint, CurlExitOperationTimeout),
			nil, triple)
	}

	// Leg 2: positive control, from the same (restricted) pod.
	if triple.PositiveControl.ExitCode != CurlExitSuccess {
		return classify("", false,
			fmt.Sprintf("positive control leg exited %d against %q — the component's network path may be broken entirely, so the denial proves nothing", triple.PositiveControl.ExitCode, triple.PositiveControl.Endpoint),
			nil, triple)
	}
	for _, model := range triple.ModelEndpoints {
		if sharesPrefix(triple.PositiveControl.Endpoint, model) {
			return classify("", false,
				fmt.Sprintf("positive control endpoint %q shares a prefix with model endpoint %q — a shared-prefix control is itself the bypass measurement this clause exists to prevent", triple.PositiveControl.Endpoint, model),
				nil, triple)
		}
	}

	// Leg 3: second-runner-class control, reaching the SAME denied target.
	if triple.ControlVantage.Endpoint != triple.Denial.Endpoint {
		return invalid(fmt.Sprintf("control-vantage leg probed %q, not the denied endpoint %q — attribution to the restricted class is unproven", triple.ControlVantage.Endpoint, triple.Denial.Endpoint), triple)
	}
	if triple.ControlVantage.ExitCode != CurlExitSuccess {
		return classify("", false,
			fmt.Sprintf("control-vantage leg exited %d against %q — the denied host may simply be down, and a down host produces the same bare denial a policy does", triple.ControlVantage.ExitCode, triple.ControlVantage.Endpoint),
			nil, triple)
	}

	return classify("", true, "", triple, nil)
}
