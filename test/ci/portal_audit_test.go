package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPortalAuditClassification(t *testing.T) {
	failed := errors.New("npm audit exited 1")
	for _, test := range []struct {
		name    string
		output  string
		err     error
		outcome portalAuditOutcome
	}{
		{name: "success", output: `{"metadata":{"vulnerabilities":{"total":0}}}`, outcome: portalAuditPassed},
		{name: "findings", output: `{"metadata":{"vulnerabilities":{"low":2,"total":2}}}`, err: failed, outcome: portalAuditFindings},
		{name: "503 unavailable", output: `{"error":{"code":"E503","summary":"503 Service Unavailable"}}`, err: failed, outcome: portalAuditUnavailable},
		{name: "network unavailable", output: `npm error code EAI_AGAIN`, err: failed, outcome: portalAuditUnavailable},
		{name: "configuration failure", output: `npm error code ELOCKVERIFY`, err: failed, outcome: portalAuditFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyPortalAudit([]byte(test.output), test.err); got != test.outcome {
				t.Fatalf("outcome = %v, want %v", got, test.outcome)
			}
		})
	}
}

func TestPortalAuditUnavailablePassesWithWarningAfterBoundedRetries(t *testing.T) {
	var stdout, stderr bytes.Buffer
	attempts := 0
	var delays []time.Duration
	code := runPortalAudit("npm", &stdout, &stderr, func(string) ([]byte, error) {
		attempts++
		return []byte(`{"error":{"code":"E503","summary":"503 Service Unavailable"}}`), errors.New("exit status 1")
	}, func(delay time.Duration) {
		delays = append(delays, delay)
	})
	if code != 0 || attempts != portalAuditAttempts {
		t.Fatalf("code=%d attempts=%d", code, attempts)
	}
	if len(delays) != portalAuditAttempts-1 || delays[0] != time.Second || delays[1] != 4*time.Second {
		t.Fatalf("delays = %v", delays)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "::warning title=Portal dependency audit unavailable::") ||
		!strings.Contains(stderr.String(), "E503 503 Service Unavailable") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestPortalAuditFindingsFailWithoutRetry(t *testing.T) {
	var stdout, stderr bytes.Buffer
	attempts := 0
	code := runPortalAudit("npm", &stdout, &stderr, func(string) ([]byte, error) {
		attempts++
		return []byte(`{"metadata":{"vulnerabilities":{"high":1,"total":1}}}`), errors.New("exit status 1")
	}, func(time.Duration) {
		t.Fatal("findings must not retry")
	})
	if code != 1 || attempts != 1 || !strings.Contains(stdout.String(), `"high":1`) {
		t.Fatalf("code=%d attempts=%d stdout=%q stderr=%q", code, attempts, stdout.String(), stderr.String())
	}
}
