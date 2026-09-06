package instance

import (
	"strings"
	"testing"
)

func TestResolveStageMemoryBound(t *testing.T) {
	tests := []struct {
		name          string
		limit         string
		wantBytes     uint64
		wantSourceHas string
		wantErr       string
	}{
		{name: "explicit limit", limit: "8Gi", wantBytes: uint64(8) << 30, wantSourceHas: "runner.stageMemoryLimit 8Gi"},
		{name: "megabyte quantity", limit: "512Mi", wantBytes: uint64(512) << 20, wantSourceHas: "512Mi"},
		{name: "unset leaves no bound", wantBytes: 0, wantSourceHas: "is not set"},
		{name: "malformed quantity is refused", limit: "8 gigabytes", wantErr: "not a Kubernetes quantity"},
		{name: "zero is refused", limit: "0", wantErr: "must be positive"},
		{name: "negative is refused", limit: "-1Gi", wantErr: "must be positive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := (&RunnerConfig{StageMemoryLimit: test.limit}).ResolveStageMemoryBound()
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error = %v, want mention of %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.MaxBytes != test.wantBytes {
				t.Errorf("MaxBytes = %d, want %d", got.MaxBytes, test.wantBytes)
			}
			if got.Enforced() != (test.wantBytes > 0) {
				t.Errorf("Enforced() = %v for MaxBytes %d", got.Enforced(), got.MaxBytes)
			}
			if !strings.Contains(got.Source, test.wantSourceHas) {
				t.Errorf("Source = %q, want mention of %q", got.Source, test.wantSourceHas)
			}
		})
	}
}

// A nil RunnerConfig must resolve, not panic: the daemon reads this on a path
// that runs before an instance is fully assembled.
func TestResolveStageMemoryBoundOnNilConfig(t *testing.T) {
	got, err := (*RunnerConfig)(nil).ResolveStageMemoryBound()
	if err != nil {
		t.Fatal(err)
	}
	if got.Enforced() {
		t.Errorf("a nil config resolved an enforced bound of %d", got.MaxBytes)
	}
}

// THE REASON THERE IS NO DERIVED DEFAULT, pinned as a test so the reasoning
// cannot quietly be undone by someone adding an "obvious" one later.
//
// The natural default — pod cgroup limit less a daemon reserve — does not
// survive the incident's measurements. This asserts the arithmetic that
// disqualified it, using the production pod limit and the recorded anon-rss of
// the children that actually killed the daemon.
func TestPodLimitLessReserveWouldNotHaveStoppedTheIncident(t *testing.T) {
	const podLimit = uint64(10) << 30 // the production Deployment's memory limit
	const reserve = uint64(1) << 30   // a generous ~3.5x the daemon's observed peak anon

	derived := podLimit - reserve
	// anon-rss recorded in #4070 and its state-of-repo review comment.
	victims := map[string]uint64{
		"7.4Gi stage burst (2026-08-31T19:58)": 7_400_000_000,
		"5,070,212 kB child":                   5_070_212 * 1024,
		"9,824,228 kB MainThread":              9_824_228 * 1024,
		"10,113,144 kB child":                  10_113_144 * 1024,
	}
	var escaped []string
	for name, rss := range victims {
		if rss <= derived {
			escaped = append(escaped, name)
		}
	}
	if len(escaped) == 0 {
		t.Fatalf("a %d-byte derived bound would have stopped every recorded victim; "+
			"if that is now true, the no-derived-default reasoning in stagememory.go needs revisiting", derived)
	}
	// Two of the four escape, which is why the default was dropped: those
	// children killed the daemon in company with page cache, the daemon
	// itself, and concurrent sibling stages — not on their own.
	if len(escaped) != 2 {
		t.Errorf("%d recorded victims escape a %d-byte derived bound (%v); the documented reasoning says 2",
			len(escaped), derived, escaped)
	}
}
