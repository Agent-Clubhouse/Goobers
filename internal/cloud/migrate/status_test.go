package migrate

import "testing"

func TestMinimumCompatibleBinaryUsesHighestAppliedFloor(t *testing.T) {
	applied := []Applied{
		{Version: 1, MinCompatibleBinary: "0.6.0"},
		{Version: 2, MinCompatibleBinary: "0.8.0"},
		{Version: 3, MinCompatibleBinary: "0.7.2"},
	}
	if got := minimumCompatibleBinary(applied); got != "0.8.0" {
		t.Fatalf("minimum compatible binary = %q, want 0.8.0", got)
	}
}
