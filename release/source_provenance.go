package main

import (
	"debug/buildinfo"
	"fmt"
	"os/exec"
	"regexp"
	"runtime/debug"
	"strings"
)

var fullSourceCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Official publication supplies a peeled, authorized full source identity.
// Ordinary local packaging can remain diagnostic by omitting -source-commit.
func verifyReleaseSource(source string) error {
	if source == "" {
		return nil
	}
	if !fullSourceCommit.MatchString(source) || gitOutput("rev-parse", "HEAD") != source {
		return fmt.Errorf("release checkout does not match the authorized full source commit")
	}
	status, err := exec.Command("git", "status", "--porcelain=v1", "--untracked-files=all").Output()
	if err != nil {
		return fmt.Errorf("cannot verify release source cleanliness: %w", err)
	}
	if len(status) != 0 {
		return fmt.Errorf("release source has tracked or untracked changes; stage build inputs outside the checkout")
	}
	return nil
}

// Read the executable as data; cross-platform binaries are never executed.
// The compiler's VCS state must agree with the pre-build source check, not a
// caller-supplied clean flag. Missing metadata is a failure, too.
func verifyReleaseBinary(binary, source, pkg string, target Target) error {
	if source == "" {
		return nil
	}
	info, err := buildinfo.ReadFile(binary)
	if err != nil {
		return fmt.Errorf("read %s release build metadata: %w", target, err)
	}
	if err := validateReleaseBuildInfo(info, source, pkg, target); err != nil {
		return fmt.Errorf("verify %s release build metadata: %w", target, err)
	}
	return nil
}

func validateReleaseBuildInfo(info *debug.BuildInfo, source, pkg string, target Target) error {
	const module = "github.com/goobers/goobers"
	if info == nil || !fullSourceCommit.MatchString(source) || !strings.HasPrefix(pkg, "./cmd/") ||
		info.Main.Path != module || info.Path != module+strings.TrimPrefix(pkg, ".") {
		return fmt.Errorf("unexpected release module or command identity")
	}
	settings := make(map[string]string)
	for _, setting := range info.Settings {
		if _, duplicate := settings[setting.Key]; duplicate {
			return fmt.Errorf("duplicate release build metadata setting")
		}
		settings[setting.Key] = setting.Value
	}
	if settings["vcs"] != "git" || settings["vcs.revision"] != source || settings["vcs.modified"] != "false" ||
		settings["GOOS"] != target.OS || settings["GOARCH"] != target.Arch {
		return fmt.Errorf("release binary must contain the exact full Git revision, explicit clean state and target platform")
	}
	return nil
}
