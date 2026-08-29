package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func touchFile(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// TestDetectCICommandDefault is #2071's stack-detection core: presence of a
// recognized build manifest seeds a stack-appropriate ciCommand default
// instead of the previously-unconditional `make ci`, and an unrecognized
// directory forces an explicit choice (empty stack/command).
func TestDetectCICommandDefault(t *testing.T) {
	tests := []struct {
		name      string
		files     []string
		wantStack string
		wantCmd   []string
		wantCap   string
	}{
		{"go.mod only", []string{"go.mod"}, "Go", []string{"go", "test", "./..."}, "go@1.26"},
		{"Makefile + go.mod: Makefile wins (#2071's own framing)", []string{"Makefile", "go.mod"}, "Makefile", []string{"make", "ci"}, "make"},
		{"lowercase makefile", []string{"makefile"}, "Makefile", []string{"make", "ci"}, "make"},
		{"GNUmakefile", []string{"GNUmakefile"}, "Makefile", []string{"make", "ci"}, "make"},
		{"csproj", []string{"Widget.csproj"}, ".NET", []string{"dotnet", "test"}, "dotnet@9"},
		{"sln", []string{"Widget.sln"}, ".NET", []string{"dotnet", "test"}, "dotnet@9"},
		{"package.json", []string{"package.json"}, "Node.js", []string{"npm", "run", "ci"}, "node@20"},
		{"pom.xml", []string{"pom.xml"}, "Maven", []string{"mvn", "-B", "-q", "verify"}, "java@21"},
		{"build.gradle", []string{"build.gradle"}, "Gradle", []string{"gradle", "check"}, "java@21"},
		{"build.gradle.kts", []string{"build.gradle.kts"}, "Gradle", []string{"gradle", "check"}, "java@21"},
		{"Cargo.toml", []string{"Cargo.toml"}, "Rust", []string{"cargo", "test"}, "rust"},
		{"Package.swift", []string{"Package.swift"}, "Swift", []string{"swift", "test"}, "swift"},
		{"pyproject.toml", []string{"pyproject.toml"}, "Python", []string{"python3", "-m", "pytest", "-q"}, "python@3.12"},
		{"setup.py", []string{"setup.py"}, "Python", []string{"python3", "-m", "pytest", "-q"}, "python@3.12"},
		{"requirements.txt", []string{"requirements.txt"}, "Python", []string{"python3", "-m", "pytest", "-q"}, "python@3.12"},
		{"no recognized manifest -> forces explicit choice", []string{"README.md"}, "", nil, ""},
		{"empty directory -> forces explicit choice", nil, "", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tt.files {
				touchFile(t, dir, f)
			}
			stack, cmd, capability := detectCICommandDefault(dir)
			if stack != tt.wantStack || !slices.Equal(cmd, tt.wantCmd) || capability != tt.wantCap {
				t.Fatalf("detectCICommandDefault(%v) = (%q, %v, %q), want (%q, %v, %q)", tt.files, stack, cmd, capability, tt.wantStack, tt.wantCmd, tt.wantCap)
			}
		})
	}

	t.Run("nonexistent directory does not error, forces explicit choice", func(t *testing.T) {
		stack, cmd, capability := detectCICommandDefault(filepath.Join(t.TempDir(), "does-not-exist"))
		if stack != "" || cmd != nil || capability != "" {
			t.Fatalf("detectCICommandDefault(nonexistent) = (%q, %v, %q), want (\"\", nil, \"\")", stack, cmd, capability)
		}
	})

	t.Run("empty dir argument forces explicit choice", func(t *testing.T) {
		stack, cmd, capability := detectCICommandDefault("")
		if stack != "" || cmd != nil || capability != "" {
			t.Fatalf("detectCICommandDefault(\"\") = (%q, %v, %q), want (\"\", nil, \"\")", stack, cmd, capability)
		}
	})

	t.Run("a directory is never matched by suffix (only files)", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "Widget.csproj"), 0o755); err != nil {
			t.Fatal(err)
		}
		stack, cmd, capability := detectCICommandDefault(dir)
		if stack != "" || cmd != nil || capability != "" {
			t.Fatalf("detectCICommandDefault matched a directory named like a manifest: (%q, %v, %q)", stack, cmd, capability)
		}
	})
}
