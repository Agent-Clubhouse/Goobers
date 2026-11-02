package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/temporalops"
	"github.com/goobers/goobers/internal/workercontract"
)

func TestTemporalWorkerContractRegisteredReadOnlyCommand(t *testing.T) {
	unsetRunContext(t)
	code, stdout, stderr := runArgs(t, "temporal", "worker-contract")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	var report temporalops.ContractReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatal(err)
	}
	if report.Manifest.SchemaVersion != workercontract.SchemaVersion || !strings.HasPrefix(report.Manifest.ExecutableDigest, "sha256:") || report.Inspection != nil {
		t.Fatalf("report = %+v", report)
	}
}
