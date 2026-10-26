package validate

import "testing"

func TestLiveDSL21RequiresExplicitPreviewAcknowledgement(t *testing.T) {
	for _, allowPreview := range []bool{false, true} {
		report := &Report{}
		checkWorkflowDSLVersion(report, dslWorkflow("watcher", "2.1"), "watcher.yaml", allowPreview)
		want := ErrorPreviewDSLVersionBlocked
		if allowPreview {
			want = WarningPreviewDSLVersionOptedIn
		}
		if len(report.Issues) != 1 || report.Issues[0].Code != want {
			t.Fatalf("preview=%t: %+v", allowPreview, report.Issues)
		}
		if report.HasErrors() == allowPreview {
			t.Fatalf("preview=%t: wrong admission %+v", allowPreview, report.Issues)
		}
	}
}
