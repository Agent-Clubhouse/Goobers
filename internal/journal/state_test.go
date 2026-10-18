package journal

import "testing"

func TestRunPhaseIsTerminal(t *testing.T) {
	for _, tc := range []struct {
		phase RunPhase
		want  bool
	}{
		{PhaseRunning, false},
		{PhaseCompleted, true},
		{PhaseFailed, true},
		{PhaseAborted, true},
		{PhaseEscalated, true},
		{"", false},
		{"bogus", false},
	} {
		if got := tc.phase.IsTerminal(); got != tc.want {
			t.Errorf("RunPhase(%q).IsTerminal() = %v, want %v", tc.phase, got, tc.want)
		}
	}
}
