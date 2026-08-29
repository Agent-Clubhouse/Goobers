package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestWindowsLargeRepoEnvironmentWarning(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{{LargeRepo: true}}}
	cases := []struct {
		name       string
		hostOS     string
		output     string
		probeErr   error
		want       string
		wantCalled bool
	}{
		{name: "non-Windows", hostOS: "linux"},
		{name: "Dev Drive", hostOS: "windows", output: "DEVDRIVE=true\nDEFENDER=false\n", wantCalled: true},
		{name: "Defender exclusion", hostOS: "windows", output: "DEVDRIVE=false\nDEFENDER=true\n", wantCalled: true},
		{name: "unoptimized", hostOS: "windows", output: "DEVDRIVE=false\nDEFENDER=false\n", want: "neither on a Windows Dev Drive nor excluded", wantCalled: true},
		{name: "inspection unavailable", hostOS: "windows", output: "DEVDRIVE=unknown\nDEFENDER=unknown\n", want: "could not verify", wantCalled: true},
		{name: "probe failure", hostOS: "windows", probeErr: errors.New("PowerShell unavailable"), want: "could not inspect", wantCalled: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			deps := windowsLargeRepoPreflightDeps{
				hostOS: tc.hostOS,
				probe: func(context.Context, string) ([]byte, error) {
					called = true
					return []byte(tc.output), tc.probeErr
				},
			}
			got := windowsLargeRepoEnvironmentWarning(cfg, t.TempDir(), deps)
			if tc.want == "" && got != "" {
				t.Fatalf("warning = %q, want none", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("warning = %q, want it to contain %q", got, tc.want)
			}
			if called != tc.wantCalled {
				t.Fatalf("probe called = %v, want %v", called, tc.wantCalled)
			}
		})
	}
}

func TestWindowsLargeRepoEnvironmentWarningSkipsOrdinaryRepos(t *testing.T) {
	called := false
	got := windowsLargeRepoEnvironmentWarning(
		&instance.Config{Repos: []instance.RepoRef{{LargeRepo: false}}},
		t.TempDir(),
		windowsLargeRepoPreflightDeps{
			hostOS: "windows",
			probe: func(context.Context, string) ([]byte, error) {
				called = true
				return nil, nil
			},
		},
	)
	if got != "" || called {
		t.Fatalf("warning = %q, probe called = %v; want ordinary repo skipped", got, called)
	}
}
