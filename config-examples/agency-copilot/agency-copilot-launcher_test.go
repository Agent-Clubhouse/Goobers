package main

import "testing"

func TestUseDirectCopilot(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "version flag", args: []string{"--version"}, want: true},
		{name: "version command", args: []string{"version"}, want: true},
		{name: "SDK model discovery", args: []string{"--headless", "--no-auto-update", "--stdio"}, want: true},
		{name: "headless prompt", args: []string{"--headless", "-p", "hello"}, want: false},
		{name: "normal prompt", args: []string{"-p", "hello"}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := useDirectCopilot(test.args); got != test.want {
				t.Fatalf("useDirectCopilot(%q) = %v, want %v", test.args, got, test.want)
			}
		})
	}
}

func TestCommandFromEnv(t *testing.T) {
	t.Setenv("AGENCY_BIN", "custom-agency")
	if got := commandFromEnv("AGENCY_BIN", "agency"); got != "custom-agency" {
		t.Fatalf("commandFromEnv() = %q, want custom-agency", got)
	}
}
