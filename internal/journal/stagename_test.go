package journal

import "testing"

func TestStageArtifactNameStripsOnlyRunQualifier(t *testing.T) {
	for _, tc := range []struct{ runID, recorded, want string }{
		{"r1", "r1:implement/result", "implement/result"},
		{"r1", "implement/result", "implement/result"},
		{"r1", "r1x:implement/result", "r1x:implement/result"},
		{"", ":implement/result", ":implement/result"},
		{"r1", "context/implement-attempt-1.json", "context/implement-attempt-1.json"},
	} {
		if got := StageArtifactName(tc.runID, tc.recorded); got != tc.want {
			t.Errorf("StageArtifactName(%q, %q) = %q, want %q", tc.runID, tc.recorded, got, tc.want)
		}
	}
}
