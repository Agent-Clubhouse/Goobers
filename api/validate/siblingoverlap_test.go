package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSiblingOverlapRoutingWarnsWithoutSequencing exercises #5592 against
// the shipped acme-web merge-review: an overlap gate whose overlap outcome
// terminates draws WF028, while the same gate routed through elect-lander
// (and from there apply-verdict) validates cleanly.
func TestSiblingOverlapRoutingWarnsWithoutSequencing(t *testing.T) {
	tests := []struct {
		name          string
		overlapTarget string
		wantWarn      bool
	}{
		{name: "overlap terminates", overlapTarget: `""`, wantWarn: true},
		{name: "overlap elects a lander", overlapTarget: "elect-lander"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.CopyFS(root, os.DirFS("../../config-examples")); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "gaggles", "acme-web", "workflows", "merge-review.yaml")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			content := strings.ReplaceAll(string(raw), "\r\n", "\n")
			const gatherNext = "      next: review\n\n    - name: apply-verdict\n"
			if !strings.Contains(content, gatherNext) {
				t.Fatalf("acme-web merge-review no longer routes gather-sibling-context to review; update this fixture")
			}
			content = strings.Replace(content, gatherNext, "      next: overlap-gate\n\n    - name: apply-verdict\n", 1)
			overlapGate := "  gates:\n" +
				"    - name: overlap-gate\n" +
				"      evaluator: automated\n" +
				"      automated:\n" +
				"        check: output-equals\n" +
				"        params:\n" +
				"          key: hasSiblingOverlap\n" +
				"          equals: \"false\"\n" +
				"      branches:\n" +
				"        pass: review\n" +
				"        fail: " + tt.overlapTarget + "\n"
			if !strings.Contains(content, "\n  gates:\n") {
				t.Fatal("acme-web merge-review has no gates section")
			}
			content = strings.Replace(content, "\n  gates:\n", "\n"+overlapGate, 1)
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			report, err := newV(t).ValidateDir(root)
			if err != nil {
				t.Fatalf("ValidateDir: %v", err)
			}
			if report.HasErrors() {
				t.Fatalf("overlap routing must not fail validation:\n%s", joinIssues(report))
			}
			var got []CodedWarning
			for _, warning := range report.Warnings() {
				if warning.Code == WarningSiblingOverlapUnsequenced {
					got = append(got, warning)
				}
			}
			if !tt.wantWarn {
				if len(got) != 0 {
					t.Fatalf("WF028 warnings = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("WF028 warnings = %+v, want exactly one", got)
			}
			if !strings.Contains(got[0].Explanation, `gate "overlap-gate"`) {
				t.Fatalf("WF028 explanation = %q, want the overlap gate named", got[0].Explanation)
			}
		})
	}
}
