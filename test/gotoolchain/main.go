// Command gotoolchain asserts the container image builds with the exact Go
// toolchain go.mod declares.
//
// Three places choose a Go toolchain: go.mod's `go` directive (the declared
// source of truth), .github/workflows/ci.yml (which defers to it via
// go-version-file: go.mod), and packaging/docker/Dockerfile's GO_IMAGE build
// argument. The Dockerfile is the only one that can drift, and it drifted:
// a floating `golang:1.26` tag built the shipped image with whatever patch
// release the registry served that day, with nothing in the diff to say so
// (#3452).
//
// Documenting the obligation would not hold — this check makes it
// self-enforcing, so a go.mod bump fails the merge gate until the Dockerfile
// follows in the same diff.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	goModPath      = "go.mod"
	dockerfilePath = "packaging/docker/Dockerfile"
	goImageArg     = "GO_IMAGE"
)

// goVersion matches a Go release version: two components (1.27) or three
// (1.26.6). go.mod may declare either; the image tag must equal whichever it
// declares.
var goVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

func main() {
	if err := verify("."); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "gotoolchain: %v\n", err)
		os.Exit(1)
	}
}

// verify reads both files from a repository root and reports any disagreement.
func verify(root string) error {
	module, err := os.ReadFile(filepath.Join(root, goModPath))
	if err != nil {
		return fmt.Errorf("read %s: %w", goModPath, err)
	}
	dockerfile, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(dockerfilePath)))
	if err != nil {
		return fmt.Errorf("read %s: %w", dockerfilePath, err)
	}

	declared, err := goDirectiveVersion(string(module))
	if err != nil {
		return fmt.Errorf("%s: %w", goModPath, err)
	}
	image, err := goImageVersion(string(dockerfile))
	if err != nil {
		return fmt.Errorf("%s: %w", dockerfilePath, err)
	}
	return compareVersions(declared, image)
}

// compareVersions fails when the two toolchains disagree, naming both values
// and the file that has to change. go.mod is the declared source of truth, so
// the Dockerfile is always the side that follows: a discrepancy says two things
// disagree, never which one is wrong, and pinning the correct leg to the
// drifting one would make the defect permanent and undetectable.
func compareVersions(declared, image string) error {
	if declared == image {
		return nil
	}
	return fmt.Errorf(
		"go toolchain drift: %s declares `go %s` but %s builds with golang:%s (ARG %s); "+
			"%s is the source of truth, so pin ARG %s in %s to golang:%s in the same commit as the go.mod change",
		goModPath, declared, dockerfilePath, image, goImageArg,
		goModPath, goImageArg, dockerfilePath, declared,
	)
}

// goDirectiveVersion extracts the version from go.mod's `go` directive. The
// `toolchain` directive is deliberately ignored: `go` is the version the module
// declares it is built for, and the one ci.yml's go-version-file resolves.
func goDirectiveVersion(content string) (string, error) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(stripComment(line, "//"))
		if len(fields) < 2 || fields[0] != "go" {
			continue
		}
		version := fields[1]
		if !goVersion.MatchString(version) {
			return "", fmt.Errorf("go directive declares unparseable version %q", version)
		}
		return version, nil
	}
	return "", fmt.Errorf("no `go` directive found")
}

// goImageVersion extracts the Go version from the Dockerfile's GO_IMAGE build
// argument default (e.g. docker.io/library/golang:1.26.6 -> 1.26.6). A tag
// carrying a base-image variant (1.26.6-bookworm) still yields its version; a
// tag that names no version at all (latest, or a bare digest pin) is an error
// rather than a silent pass, because this check cannot vouch for a toolchain it
// cannot read.
func goImageVersion(content string) (string, error) {
	reference, ok := goImageReference(content)
	if !ok {
		return "", fmt.Errorf("no `ARG %s=` default found", goImageArg)
	}
	// Split the tag off the final path element so a registry host:port
	// (registry:5000/library/golang:1.26.6) is not mistaken for one.
	name := reference[strings.LastIndex(reference, "/")+1:]
	separator := strings.LastIndex(name, ":")
	if separator < 0 {
		return "", fmt.Errorf("ARG %s=%s has no image tag; pin it to the version go.mod declares", goImageArg, reference)
	}
	tag := name[separator+1:]
	version, _, _ := strings.Cut(tag, "-")
	if !goVersion.MatchString(version) {
		return "", fmt.Errorf("cannot read a Go version from ARG %s tag %q; pin it to the version go.mod declares", goImageArg, tag)
	}
	return version, nil
}

// goImageReference returns the image reference the Dockerfile defaults GO_IMAGE
// to. Comments are stripped first so a commented-out pin never satisfies this.
func goImageReference(content string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		fields := strings.Fields(stripComment(line, "#"))
		if len(fields) < 2 || !strings.EqualFold(fields[0], "ARG") {
			continue
		}
		value, ok := strings.CutPrefix(fields[1], goImageArg+"=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"'`)
		if value == "" {
			continue
		}
		return value, true
	}
	return "", false
}

// stripComment removes a comment introduced by marker — `#` in the Dockerfile,
// `//` in go.mod. Docker only honours `#` at the start of a line (a trailing
// `#` would land inside the ARG value), and go.mod's directives are similarly
// whole-line, so this exists for one purpose: keeping a commented-out
// declaration from being read as a live one.
func stripComment(line, marker string) string {
	if index := strings.Index(line, marker); index >= 0 {
		return line[:index]
	}
	return line
}
