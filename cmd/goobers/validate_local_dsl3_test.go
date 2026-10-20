package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestValidateDSL3LocalPlacementAdvisories(t *testing.T) {
	for _, version := range []string{"3.0", "3.1"} {
		for _, tc := range []struct{ name, runsOn, code string }{
			{"resource omissions", "{}", "RNR004"},
			{"undeclared tag", "{cpu: 1000m, memory: 1Gi, capabilities: [unclaimed-tool]}", "RNR001"},
			{"OS pin", "{cpu: 1000m, memory: 1Gi, os: windows}", "RNR001"},
		} {
			t.Run(version+"/"+tc.name, func(t *testing.T) {
				root := initDemo(t)
				writeSecondWorkflow(t, root, fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Workflow
dslVersion: %q
metadata:
  name: win-build
  annotations:
    goobers.dev/allow-preview-features: "true"
spec:
  gaggle: example
  triggers: [{type: manual}]
  start: review
  gates:
    - name: review
      evaluator: agentic
      agentic: {goober: coder}
      runsOn: %s
      branches: {pass: "", fail: "@abort", needs-changes: "@escalate"}
`, version, tc.runsOn))
				code, stdout, stderr := runArgs(t, "validate", root)
				if code != 0 || !strings.Contains(stdout, "WARNING "+tc.code+" Workflow/win-build") {
					t.Fatalf("local validate = %d, stdout=%s stderr=%s", code, stdout, stderr)
				}
				declareInventory(t, root)
				declareRemoteRunner(t, root, "  - name: pod\n    host: ghcr.io/example/worker:v1\n    provides:\n      os: linux\n      harnesses: [copilot]\n")
				expected := tc.code
				if expected == "RNR004" {
					expected = "RNR003"
				}
				code, stdout, stderr = runArgs(t, "validate", root)
				if code != 1 || !strings.Contains(stdout, "ERROR "+expected+" Workflow/win-build") {
					t.Fatalf("distributed validate = %d, stdout=%s stderr=%s", code, stdout, stderr)
				}
			})
		}
	}
}
