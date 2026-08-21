package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGoDirectiveVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    string
		wantErr string
	}{
		{
			name:    "patch version",
			content: "module github.com/goobers/goobers\n\ngo 1.26.6\n\nrequire (\n\tgithub.com/example/go v1.2.3\n)\n",
			want:    "1.26.6",
		},
		{
			name:    "minor version only",
			content: "module m\n\ngo 1.27\n",
			want:    "1.27",
		},
		{
			name:    "toolchain directive is not the go directive",
			content: "module m\n\ntoolchain go1.26.9\n\ngo 1.26.6\n",
			want:    "1.26.6",
		},
		{
			name:    "commented-out directive is not live",
			content: "module m\n\n// go 1.26.6\ngo 1.26.7\n",
			want:    "1.26.7",
		},
		{
			name:    "unparseable version",
			content: "module m\n\ngo tip\n",
			wantErr: `unparseable version "tip"`,
		},
		{
			name:    "no directive at all",
			content: "module m\n",
			wantErr: "no `go` directive",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := goDirectiveVersion(test.content)
			assertVersion(t, got, err, test.want, test.wantErr)
		})
	}
}

func TestGoImageVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    string
		wantErr string
	}{
		{
			name:    "pinned patch version",
			content: "FROM scratch\nARG GO_IMAGE=docker.io/library/golang:1.26.6\n",
			want:    "1.26.6",
		},
		{
			name:    "floating minor is still read, and will not equal a patch pin",
			content: "ARG GO_IMAGE=docker.io/library/golang:1.26\n",
			want:    "1.26",
		},
		{
			name:    "base-image variant suffix",
			content: "ARG GO_IMAGE=golang:1.26.6-bookworm\n",
			want:    "1.26.6",
		},
		{
			name:    "registry host with a port is not mistaken for a tag",
			content: "ARG GO_IMAGE=registry.example:5000/library/golang:1.26.6\n",
			want:    "1.26.6",
		},
		{
			name:    "other ARGs are ignored",
			content: "ARG RUNTIME_IMAGE=docker.io/library/node:22-bookworm-slim\nARG GO_IMAGE=golang:1.26.6\nARG COPILOT_VERSION=1.0.80\n",
			want:    "1.26.6",
		},
		{
			name:    "quoted value",
			content: "ARG GO_IMAGE=\"golang:1.26.6\"\n",
			want:    "1.26.6",
		},
		{
			name:    "commented-out pin is not live",
			content: "# ARG GO_IMAGE=golang:1.26.6\nARG GO_IMAGE=golang:1.26.7\n",
			want:    "1.26.7",
		},
		{
			name:    "unparseable: moving tag",
			content: "ARG GO_IMAGE=docker.io/library/golang:latest\n",
			wantErr: `cannot read a Go version from ARG GO_IMAGE tag "latest"`,
		},
		{
			name:    "unparseable: no tag at all",
			content: "ARG GO_IMAGE=docker.io/library/golang\n",
			wantErr: "has no image tag",
		},
		{
			name:    "unparseable: digest pin carries no version",
			content: "ARG GO_IMAGE=docker.io/library/golang@sha256:abc123\n",
			wantErr: `cannot read a Go version from ARG GO_IMAGE tag "abc123"`,
		},
		{
			name:    "missing argument",
			content: "FROM docker.io/library/golang:1.26.6\n",
			wantErr: "no `ARG GO_IMAGE=` default found",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := goImageVersion(test.content)
			assertVersion(t, got, err, test.want, test.wantErr)
		})
	}
}

func TestCompareVersions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		declared string
		image    string
		wantErr  bool
	}{
		{name: "match", declared: "1.26.6", image: "1.26.6"},
		{name: "patch drift", declared: "1.26.6", image: "1.26.7", wantErr: true},
		{name: "floating minor", declared: "1.26.6", image: "1.26", wantErr: true},
		{name: "minor drift", declared: "1.26.6", image: "1.27.0", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := compareVersions(test.declared, test.image)
			if !test.wantErr {
				if err != nil {
					t.Fatalf("compareVersions(%q, %q) = %v, want nil", test.declared, test.image, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("compareVersions(%q, %q) = nil, want a drift failure", test.declared, test.image)
			}
			// The message has to be actionable on its own: both values, and
			// the file to change. A failure naming only one side leaves the
			// reader to guess which leg is wrong.
			for _, want := range []string{test.declared, test.image, dockerfilePath, goModPath, goImageArg} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("drift message %q does not name %q", err, want)
				}
			}
		})
	}
}

// TestVerifyReportsDrift drives the whole file-reading path, not just the
// parsers: a check that only proves its comparator works has not proved it
// reads the files the merge gate cares about.
func TestVerifyReportsDrift(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, goModPath), "module m\n\ngo 1.26.6\n")
	writeFile(t, filepath.Join(root, filepath.FromSlash(dockerfilePath)), "ARG GO_IMAGE=docker.io/library/golang:1.26.7\n")

	err := verify(root)
	if err == nil {
		t.Fatal("verify = nil, want a drift failure")
	}
	if !strings.Contains(err.Error(), "1.26.6") || !strings.Contains(err.Error(), "1.26.7") {
		t.Fatalf("verify = %v, want both versions named", err)
	}

	writeFile(t, filepath.Join(root, filepath.FromSlash(dockerfilePath)), "ARG GO_IMAGE=docker.io/library/golang:1.26.6\n")
	if err := verify(root); err != nil {
		t.Fatalf("verify after pinning = %v, want nil", err)
	}
}

func TestVerifyReportsMissingFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	if err := verify(root); err == nil || !strings.Contains(err.Error(), "read go.mod") {
		t.Fatalf("verify with no go.mod = %v, want a read failure", err)
	}

	writeFile(t, filepath.Join(root, goModPath), "module m\n\ngo 1.26.6\n")
	if err := verify(root); err == nil || !strings.Contains(err.Error(), "read "+dockerfilePath) {
		t.Fatalf("verify with no Dockerfile = %v, want a read failure", err)
	}
}

// TestRepositoryPinsGoImageToGoMod is the gate itself, run against the real
// tree so `go test ./test/...` catches drift as well as the merge-gate check.
func TestRepositoryPinsGoImageToGoMod(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if err := verify(root); err != nil {
		t.Fatal(err)
	}
}

func assertVersion(t *testing.T, got string, err error, want, wantErr string) {
	t.Helper()
	if wantErr != "" {
		if err == nil {
			t.Fatalf("version = %q, want error containing %q", got, wantErr)
		}
		if !strings.Contains(err.Error(), wantErr) {
			t.Fatalf("error = %v, want it to contain %q", err, wantErr)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != want {
		t.Fatalf("version = %q, want %q", got, want)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
