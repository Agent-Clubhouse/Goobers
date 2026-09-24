package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestUpgradeDelegatesExactSelectionWithoutChangingUpdater(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(root, "Programs", "Goobers.MS", "CurrentVersion", "Goobers.MS.Setup.exe")
	if err := os.MkdirAll(filepath.Dir(executable), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		args []string
		want []string
	}{
		{"saved channel", nil, []string{"--update"}},
		{"explicit version", []string{"--version", "v0.5.0-beta.2"}, []string{"--update", "--goobers-version", "v0.5.0-beta.2"}},
		{"switch channel", []string{"--channel", "dogfood"}, []string{"--update", "--channel", "dogfood"}},
		{"selected instance", []string{"--version", "v0.5.0", root}, []string{"--update", "--goobers-version", "v0.5.0", "--instance", root}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			called := false
			code := runUpgradeWith(test.args, &stdout, &stderr, "windows", root,
				func(path string, args []string) error {
					called = true
					if path != executable || !reflect.DeepEqual(args, test.want) {
						t.Fatalf("launch = %s %q, want %s %q", path, args, executable, test.want)
					}
					return nil
				})
			if code != 0 || !called || stderr.Len() != 0 {
				t.Fatalf("code=%d called=%t stderr=%s", code, called, stderr.String())
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := runUpgradeWith(nil, &stdout, &stderr, "windows", root,
		func(string, []string) error { return errors.New("coordinator failed") }); code != 1 ||
		!strings.Contains(stderr.String(), "coordinator failed") {
		t.Fatalf("coordinator failure: code=%d stderr=%s", code, stderr.String())
	}
}

func TestUpgradeRejectsUnmanagedAndInvalidRequests(t *testing.T) {
	for _, test := range []struct {
		name, goos string
		args       []string
	}{
		{"other platform", "linux", nil},
		{"missing installation", "windows", nil},
		{"unknown channel", "windows", []string{"--channel", "preview"}},
		{"extra paths", "windows", []string{"one", "two"}},
		{"unknown flag", "windows", []string{"--force"}},
		{"empty version", "windows", []string{"--version", ""}},
		{"empty channel", "windows", []string{"--channel", " "}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runUpgradeWith(test.args, &stdout, &stderr, test.goos, t.TempDir(),
				func(string, []string) error { t.Fatal("unexpected launch"); return nil })
			if code == 0 || stderr.Len() == 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
		})
	}
}
