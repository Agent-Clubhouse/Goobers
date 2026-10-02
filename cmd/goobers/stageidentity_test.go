package main

import (
	"strings"
	"testing"
)

func TestStatusReportsStageServiceAccounts(t *testing.T) {
	root := initDemo(t)
	for _, args := range [][]string{{"status", root}, {"status", "--json", root}} {
		code, stdout, stderr := runArgs(t, args...)
		if code != 0 || !strings.Contains(stdout, "goobers-stage") {
			t.Fatalf("args=%v code=%d stdout=%s stderr=%s", args, code, stdout, stderr)
		}
	}
}

func TestDoctorAdmissionScopeFlags(t *testing.T) {
	withFakeDoctorCluster(t)
	code, stdout, stderr := runArgs(t, "doctor", "--k8s", "--checks", "pod-security-admission", "--psa-namespaces", "missing", "--psa-service-account", "custom")
	if code != 0 || !strings.Contains(stdout, "missing: unchecked:") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	code, _, stderr = runArgs(t, "doctor", "--repo", "--psa-namespaces", "missing")
	if code != 2 || !strings.Contains(stderr, "require --k8s") {
		t.Fatalf("code=%d stderr=%s", code, stderr)
	}
}
