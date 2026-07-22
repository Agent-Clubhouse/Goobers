//go:build unix

package main

import (
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestRunIDPattern(t *testing.T) {
	out := "created run a671b69fe766595e550677b91658726a (workflow=demo gaggle=demo)\nfinished: phase=completed\n"
	m := runIDPattern.FindStringSubmatch(out)
	if m == nil {
		t.Fatal("expected a run id match")
	}
	if got, want := m[1], "a671b69fe766595e550677b91658726a"; got != want {
		t.Fatalf("run id = %q, want %q", got, want)
	}
}

func TestRunIDPatternNoMatch(t *testing.T) {
	if runIDPattern.FindStringSubmatch("no run here") != nil {
		t.Fatal("did not expect a match")
	}
}

func TestFirstLine(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"one\ntwo\n", "one"},
		{"  solo  ", "solo"},
		{"", ""},
	} {
		if got := firstLine(tc.in); got != tc.want {
			t.Fatalf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestConfigureEphemeralAPI(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "instance.yaml")
	if err := instance.WriteConfig(path, &instance.Config{
		APIVersion: instance.ConfigAPIVersion,
		Kind:       instance.ConfigKind,
	}); err != nil {
		t.Fatal(err)
	}

	if err := configureEphemeralAPI(root); err != nil {
		t.Fatal(err)
	}
	cfg, err := instance.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.API.Listen != ephemeralAPIListenAddress {
		t.Fatalf("API listen = %q, want %q", cfg.API.Listen, ephemeralAPIListenAddress)
	}
}
