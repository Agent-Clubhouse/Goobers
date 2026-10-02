package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNativeSandboxHidesControllerKeysAndPreservesGrantedCredentials(t *testing.T) {
	base := requiredNativeSandbox(t)
	workspace := t.TempDir()
	private := t.TempDir()
	key := filepath.Join(private, "controller.key")
	keyDirectory := filepath.Join(private, "wrapping-keys")
	if err := os.Mkdir(keyDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	version := filepath.Join(keyDirectory, "v1.pem")
	for path, secret := range map[string]string{key: "fixture-controller-private", version: "fixture-wrapping-private"} {
		if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(workspace, "key-link")
	if err := os.Symlink(key, alias); err != nil {
		t.Fatal(err)
	}
	granted := filepath.Join(workspace, "granted-token")
	if err := os.WriteFile(granted, []byte("fixture-granted-file"), 0600); err != nil {
		t.Fatal(err)
	}
	paths := []string{key, keyDirectory}
	s := WithReadDenials(base, paths)
	paths[0] = granted // The policy owns its snapshot, not this caller slice.
	for range 2 {      // Initial and recovery commands retain the same denials.
		command := exec.Command("sh", "-c", `for path do cat "$path" 2>/dev/null || :; done; printf '\n%s\n' "$STAGE_GRANTED_TOKEN"`, "guarded-read", key, alias, version, granted)
		command.Dir = workspace
		command.Env = []string{"PATH=/usr/bin:/bin", "STAGE_GRANTED_TOKEN=fixture-granted-env"}
		if err := s.Wrap(command, Policy{Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("guarded child: %v %s", err, output)
		}
		text := string(output)
		if strings.Contains(text, "fixture-controller-private") || strings.Contains(text, "fixture-wrapping-private") {
			t.Fatal("guarded child obtained controller key contents")
		}
		if !strings.Contains(text, "fixture-granted-file") || !strings.Contains(text, "fixture-granted-env") {
			t.Fatalf("explicitly granted credentials lost: %s", text)
		}
	}
}

func TestReadDenialsRefuseWritableAndAmbiguousPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native policy boundary is Unix-only")
	}
	workspace := t.TempDir()
	private := t.TempDir()
	key := filepath.Join(private, "controller.key")
	if err := os.WriteFile(key, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name            string
		paths, writable []string
	}{
		{"empty", []string{""}, []string{workspace}},
		{"missing", []string{filepath.Join(private, "missing")}, []string{workspace}},
		{"writable-parent", []string{key}, []string{workspace, private}},
		{"workspace-hidden", []string{workspace}, []string{workspace}},
		{"too-many", make([]string, maxReadDeniedPaths+1), []string{workspace}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := validate(exec.Command("/bin/true"), Policy{Workspace: workspace, WritableRoots: test.writable, ReadDeniedPaths: test.paths})
			if err == nil {
				t.Fatal("unsafe guard accepted")
			}
			if strings.Contains(err.Error(), private) || strings.Contains(err.Error(), workspace) {
				t.Fatal("guard error disclosed host paths")
			}
		})
	}
	if err := os.Link(key, filepath.Join(workspace, "hard-link")); err != nil {
		t.Fatal(err)
	}
	if _, err := validateReadDenials([]string{key}, []string{workspace}); err == nil {
		t.Fatal("multiply-linked private file accepted")
	}
}

func TestReadDenialsCanonicalizeAndCollapseDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native policy boundary is Unix-only")
	}
	workspace := t.TempDir()
	private := t.TempDir()
	key := filepath.Join(private, "key")
	if err := os.WriteFile(key, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(private, alias); err != nil {
		t.Fatal(err)
	}
	got, err := validateReadDenials([]string{key, private, alias}, []string{workspace})
	if err != nil || len(got) != 1 || !got[0].directory {
		t.Fatalf("canonical guard: %+v %v", got, err)
	}
}

func TestNativeSandboxRefusesGuardedDirectoryWithExternalHardlink(t *testing.T) {
	base := requiredNativeSandbox(t)
	workspace := t.TempDir()
	private := t.TempDir()
	key := filepath.Join(private, "version.pem")
	if err := os.WriteFile(key, []byte("fixture-directory-private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(key, filepath.Join(workspace, "outside-key-alias")); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("sh", "-c", "exit 0")
	if err := WithReadDenials(base, []string{private}).Wrap(command, Policy{Workspace: workspace}); err != errReadDeniedPath {
		t.Fatalf("guarded directory with external alias: %v; want generic refusal before launch", err)
	}
}

func TestReadDeniedDirectoryRejectsAmbiguityAndTraversalLimits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native policy boundary is Unix-only")
	}
	t.Run("symlink", func(t *testing.T) {
		private := t.TempDir()
		if err := os.Symlink(t.TempDir(), filepath.Join(private, "alias")); err != nil {
			t.Fatal(err)
		}
		remaining := maxReadDeniedEntries
		if err := validateReadDeniedDirectory(private, 0, &remaining); err != errReadDeniedPath {
			t.Fatalf("symlink: %v; want refusal", err)
		}
	})
	t.Run("entry-budget", func(t *testing.T) {
		private := t.TempDir()
		for _, name := range []string{"first", "second"} {
			if err := os.WriteFile(filepath.Join(private, name), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		remaining := 1
		if err := validateReadDeniedDirectory(private, 0, &remaining); err != errReadDeniedPath {
			t.Fatalf("entry budget: %v; want refusal", err)
		}
	})
	t.Run("depth", func(t *testing.T) {
		private := t.TempDir()
		path := private
		for range maxReadDeniedDepth {
			path = filepath.Join(path, "nested")
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
		}
		remaining := maxReadDeniedEntries
		if err := validateReadDeniedDirectory(private, 0, &remaining); err != errReadDeniedPath {
			t.Fatalf("depth budget: %v; want refusal", err)
		}
	})
	t.Run("inspection-error", func(t *testing.T) {
		remaining := maxReadDeniedEntries
		if err := validateReadDeniedDirectory(filepath.Join(t.TempDir(), "missing"), 0, &remaining); err != errReadDeniedPath {
			t.Fatalf("inspection error: %v; want refusal", err)
		}
	})
}
