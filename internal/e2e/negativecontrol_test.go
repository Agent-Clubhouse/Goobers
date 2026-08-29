package e2e

import "testing"

func validTriple() NegativeControlTriple {
	return NegativeControlTriple{
		Denial:          NegativeControlProbe{Endpoint: "evil.example.com:443", ExitCode: CurlExitOperationTimeout},
		PositiveControl: NegativeControlProbe{Endpoint: "registry.example.com:443", ExitCode: CurlExitSuccess},
		ModelEndpoints:  []string{"api.anthropic.com:443"},
		ControlVantage:  NegativeControlProbe{Endpoint: "evil.example.com:443", ExitCode: CurlExitSuccess},
	}
}

func TestAssertNegativeControlTriplePass(t *testing.T) {
	got := AssertNegativeControlTriple(validTriple())
	if got.Verdict != VerdictPass {
		t.Fatalf("Verdict = %v, want pass; detail=%q", got.Verdict, got.Detail)
	}
}

// TestAssertNegativeControlTripleRejectsDNSFailure is S9's named
// false-green: "never exit 6 (DNS)."
func TestAssertNegativeControlTripleRejectsDNSFailure(t *testing.T) {
	triple := validTriple()
	triple.Denial.ExitCode = CurlExitCouldNotResolveHost
	got := AssertNegativeControlTriple(triple)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (DNS failure is not proof of policy denial)", got.Verdict)
	}
}

func TestAssertNegativeControlTripleRejectsWrongDenialExitCode(t *testing.T) {
	triple := validTriple()
	triple.Denial.ExitCode = 1 // generic curl error, neither 28 nor 6
	got := AssertNegativeControlTriple(triple)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (denial must be exactly exit 28)", got.Verdict)
	}
}

func TestAssertNegativeControlTripleRejectsFailedPositiveControl(t *testing.T) {
	triple := validTriple()
	triple.PositiveControl.ExitCode = CurlExitOperationTimeout
	got := AssertNegativeControlTriple(triple)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (positive control failed)", got.Verdict)
	}
}

// TestAssertNegativeControlTripleRejectsSharedPrefix is S9's explicit
// false-green: "a shared-prefix control is itself the bypass measurement."
func TestAssertNegativeControlTripleRejectsSharedPrefix(t *testing.T) {
	triple := validTriple()
	triple.PositiveControl.Endpoint = "api.anthropic.com.mirror.example.com:443"
	triple.ModelEndpoints = []string{"api.anthropic.com:443"}
	got := AssertNegativeControlTriple(triple)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (positive control shares a prefix with a model endpoint)", got.Verdict)
	}
}

func TestAssertNegativeControlTripleRejectsControlVantageMismatch(t *testing.T) {
	triple := validTriple()
	triple.ControlVantage.Endpoint = "some-other-host.example.com:443"
	got := AssertNegativeControlTriple(triple)
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid (control vantage did not target the denied endpoint)", got.Verdict)
	}
}

func TestAssertNegativeControlTripleRejectsDownControlVantage(t *testing.T) {
	triple := validTriple()
	triple.ControlVantage.ExitCode = CurlExitOperationTimeout
	got := AssertNegativeControlTriple(triple)
	if got.Verdict != VerdictFail {
		t.Fatalf("Verdict = %v, want fail (denied host may simply be down)", got.Verdict)
	}
}

func TestAssertNegativeControlTripleInvalidOnMissingEndpoints(t *testing.T) {
	got := AssertNegativeControlTriple(NegativeControlTriple{})
	if got.Verdict != VerdictInvalid {
		t.Fatalf("Verdict = %v, want invalid", got.Verdict)
	}
}

func TestSharesPrefix(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"api.anthropic.com", "api.anthropic.com.evil.example", true},
		{"api.anthropic.com.evil.example", "api.anthropic.com", true},
		{"registry.example.com", "api.anthropic.com", false},
		{"", "api.anthropic.com", false},
	}
	for _, tc := range cases {
		if got := sharesPrefix(tc.a, tc.b); got != tc.want {
			t.Fatalf("sharesPrefix(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
