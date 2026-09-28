package main

import (
	"io"
	"runtime/debug"
	"strings"
	"testing"
)

func TestReleaseSourceFlagsRequireExactIdentity(t *testing.T) {
	source := strings.Repeat("a", 40)
	base := []string{"-version=v1.2.3", "-commit=" + source[:12], "-first-feature-snapshot"}
	for _, extra := range [][]string{
		{"-source-commit=" + source},
		{"-source-commit=" + source, "-commit=" + source},
	} {
		if _, err := parseFlags(append(append([]string(nil), base...), extra...), io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	for _, extra := range [][]string{
		{"-source-commit=HEAD"}, {"-source-commit=" + source[:12]},
		{"-source-commit=" + strings.ToUpper(source)},
		{"-source-commit=" + source, "-commit=" + source[:7]},
		{"-source-commit=" + source, "-commit=" + strings.Repeat("b", 12)},
		{"-source-commit=" + source, "-skip-unbuildable"},
	} {
		if _, err := parseFlags(append(append([]string(nil), base...), extra...), io.Discard); err == nil {
			t.Fatalf("accepted invalid source options: %v", extra)
		}
	}
	args := append(importTestArgs(t.TempDir(), "linux/amd64"), "-source-commit=0123456789abcdef0123456789abcdef01234567")
	if _, err := parseFlags(args, io.Discard); err == nil || !strings.Contains(err.Error(), "does not apply") {
		t.Fatalf("artifact import accepted source-build guarantee: %v", err)
	}
}

func TestReleaseBuildMetadataRejectsDirtyMissingOrMismatchedIdentity(t *testing.T) {
	source := strings.Repeat("a", 40)
	target := Target{OS: "linux", Arch: "amd64"}
	clean := debug.BuildInfo{Path: "github.com/goobers/goobers/cmd/goobers",
		Main: debug.Module{Path: "github.com/goobers/goobers"},
		Settings: []debug.BuildSetting{{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: source},
			{Key: "vcs.modified", Value: "false"}, {Key: "GOOS", Value: "linux"}, {Key: "GOARCH", Value: "amd64"}}}
	if err := validateReleaseBuildInfo(&clean, source, "./cmd/goobers", target); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"vcs", "vcs.revision", "vcs.modified", "GOOS", "GOARCH"} {
		for _, value := range []string{"", "wrong", "true"} {
			changed := clean
			changed.Settings = append([]debug.BuildSetting(nil), clean.Settings...)
			for i := range changed.Settings {
				if changed.Settings[i].Key == key {
					changed.Settings[i].Value = value
				}
			}
			if err := validateReleaseBuildInfo(&changed, source, "./cmd/goobers", target); err == nil {
				t.Fatalf("accepted %s=%q", key, value)
			}
		}
	}
	for _, changed := range []*debug.BuildInfo{nil, {},
		{Path: clean.Path, Main: clean.Main},
		{Path: "other", Main: clean.Main, Settings: clean.Settings},
		{Path: clean.Path, Main: debug.Module{Path: "other"}, Settings: clean.Settings},
		{Path: clean.Path, Main: clean.Main, Settings: append(append([]debug.BuildSetting(nil), clean.Settings...), clean.Settings[0])},
	} {
		if err := validateReleaseBuildInfo(changed, source, "./cmd/goobers", target); err == nil {
			t.Fatal("accepted invalid metadata")
		}
	}
	for _, wrong := range []string{source[:12], source[:12] + strings.Repeat("b", 28)} {
		if err := validateReleaseBuildInfo(&clean, wrong, "./cmd/goobers", target); err == nil {
			t.Fatal("accepted incomplete or same-prefix wrong full revision")
		}
	}
	if err := validateReleaseBuildInfo(&clean, source, "./cmd/operator", target); err == nil {
		t.Fatal("accepted CLI as image operator")
	}
}
