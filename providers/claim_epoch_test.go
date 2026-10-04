package providers

import "testing"

func claimBreadcrumbWithAttribution(t *testing.T, runID, instanceID string) string {
	t.Helper()
	body, err := withAttribution(claimBreadcrumb(runID), Attribution{
		InstanceID: instanceID, Gaggle: "goobers", Workflow: "implementation",
		Task: "claim", Goober: "deterministic", Run: runID,
	}, "claim")
	if err != nil {
		t.Fatal(err)
	}
	return body
}
