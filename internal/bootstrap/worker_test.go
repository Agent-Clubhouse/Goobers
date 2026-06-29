package bootstrap

import "testing"

func TestNewStarterDefaultsTaskQueue(t *testing.T) {
	// A nil client is fine for construction (the starter only dials on Start).
	if NewStarter(nil, "") == nil {
		t.Fatal("NewStarter returned nil")
	}
	if NewStarter(nil, "custom-queue") == nil {
		t.Fatal("NewStarter with explicit queue returned nil")
	}
	if DefaultTaskQueue == "" {
		t.Fatal("DefaultTaskQueue must be set")
	}
}
