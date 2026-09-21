package fleetdiagnostics

import (
	"testing"
	"time"
)

func TestMCPConditionIndependentHealthRecoveryAndQueryIsolation(t *testing.T) {
	now := testTime
	backend := backendFixture(t, &now)
	attrs := heartbeatFields(now)
	attrs["requiredMcpState"], attrs["requiredMcpCoverage"], attrs["requiredMcpReason"] = "active", "partial", "tool_authorization_failure"
	attrs["requiredMcpActiveCount"], attrs["requiredMcpObservedAt"], attrs["requiredMcpAdapter"] = 1, now.Format(time.RFC3339Nano), "copilot-cli"
	ingest(t, backend, HeartbeatEvent, attrs)
	report := oneReport(t, backend)
	if report.State != "idle" || report.RequiredMCP == nil || report.RequiredMCP.State != "active" {
		t.Fatal(report)
	}
	*report.RequiredMCP.ActiveCount = 99
	if *oneReport(t, backend).RequiredMCP.ActiveCount != 1 {
		t.Fatal("query aliases stored condition")
	}
	now = now.Add(time.Second)
	attrs["sequence"], attrs["observedAt"], attrs["requiredMcpObservedAt"] = 2, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)
	attrs["requiredMcpState"], attrs["requiredMcpCoverage"], attrs["requiredMcpReason"], attrs["requiredMcpActiveCount"] = "recovered", "complete", "", 0
	ingest(t, backend, HeartbeatEvent, attrs)
	report = oneReport(t, backend)
	if report.RequiredMCP.State != "recovered" {
		t.Fatal(report)
	}
	found := 0
	for _, transition := range report.Transitions {
		if transition.Condition == "required_mcp" {
			found++
		}
	}
	if found != 2 {
		t.Fatalf("condition transitions missing: %+v", report.Transitions)
	}
	now = now.Add(time.Minute)
	report = oneReport(t, backend)
	if report.RequiredMCP.State != "unknown" || report.RequiredMCP.ActiveCount != nil {
		t.Fatal("missing heartbeat still asserts tool condition", report)
	}
}

func TestMCPWireRejectsFalseRecoveryAndFutureEvidence(t *testing.T) {
	for _, test := range []struct {
		state, coverage, reason string
		count                   int
		future                  bool
	}{
		{"recovered", "partial", "", 0, false}, {"active", "complete", "", 1, false},
		{"unknown", "unknown", "", 0, false}, {"active", "partial", "transport_failure", 257, false},
		{"active", "partial", "transport_failure", 1, true},
	} {
		attrs := heartbeatFields(testTime)
		at := testTime
		if test.future {
			at = at.Add(time.Second)
		}
		attrs["requiredMcpState"], attrs["requiredMcpCoverage"], attrs["requiredMcpReason"] = test.state, test.coverage, test.reason
		attrs["requiredMcpActiveCount"], attrs["requiredMcpObservedAt"] = test.count, at.Format(time.RFC3339Nano)
		if _, err := DecodeHeartbeat(attrs); err == nil {
			t.Fatalf("invalid condition accepted: %+v", test)
		}
	}
}
