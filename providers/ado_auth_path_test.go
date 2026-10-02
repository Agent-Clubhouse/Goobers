package providers

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeAzureCLITestLauncher(t *testing.T, directory, body string) string {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	name := "az"
	if runtime.GOOS == "windows" {
		name = "az.cmd"
	}
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAzureCLIExecutableCandidatesFollowPathOrderAndDeduplicate(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	if runtime.GOOS == "windows" {
		t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
		writeAzureCLITestLauncher(t, first, "@exit /b 0\r\n")
		writeAzureCLITestLauncher(t, second, "@exit /b 0\r\n")
	} else {
		writeAzureCLITestLauncher(t, first, "#!/bin/sh\nexit 0\n")
		writeAzureCLITestLauncher(t, second, "#!/bin/sh\nexit 0\n")
	}

	candidates := azureCLIExecutableCandidates(strings.Join(
		[]string{first, second, first},
		string(os.PathListSeparator),
	))
	if len(candidates) != 2 {
		t.Fatalf("candidates = %q, want two distinct launchers", candidates)
	}
	for i, directory := range []string{first, second} {
		if filepath.Dir(candidates[i]) != directory {
			t.Fatalf("candidate %d = %q, want launcher in %q", i, candidates[i], directory)
		}
	}
}

func TestAzureCLIExecutableCandidatesDeduplicateFileAliases(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target")
	alias := filepath.Join(t.TempDir(), "alias")
	if runtime.GOOS == "windows" {
		t.Skip("directory symlink creation is not reliably available to unprivileged Windows tests")
	}
	writeAzureCLITestLauncher(t, target, "#!/bin/sh\nexit 0\n")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	candidates := azureCLIExecutableCandidates(strings.Join(
		[]string{target, alias},
		string(os.PathListSeparator),
	))
	if len(candidates) != 1 {
		t.Fatalf("candidates = %q, want one executable behind two directory aliases", candidates)
	}
}

func TestAzureCLIExecRunnerUsesOnlyFirstPathCandidate(t *testing.T) {
	first := filepath.Join(t.TempDir(), "first")
	second := filepath.Join(t.TempDir(), "second")
	secondMarker := filepath.Join(t.TempDir(), "second-ran")
	if runtime.GOOS == "windows" {
		t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
		writeAzureCLITestLauncher(t, first, "@exit /b 7\r\n")
		writeAzureCLITestLauncher(t, second, "@echo ran>\""+secondMarker+"\"\r\n@exit /b 0\r\n")
	} else {
		writeAzureCLITestLauncher(t, first, "#!/bin/sh\nexit 7\n")
		writeAzureCLITestLauncher(t, second, "#!/bin/sh\necho ran > \""+secondMarker+"\"\nexit 0\n")
	}
	t.Setenv("PATH", strings.Join([]string{first, second}, string(os.PathListSeparator)))

	_, err := (azureCLIExecRunner{}).Run(context.Background(), "az", "account", "get-access-token")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 {
		t.Fatalf("error = %v, want exit code 7 from the first launcher", err)
	}
	var ambiguity *azureCLIPathAmbiguityError
	if !errors.As(err, &ambiguity) || ambiguity.candidateCount != 2 {
		t.Fatalf("error = %v, want two-candidate ambiguity", err)
	}
	if _, err := os.Stat(secondMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second launcher was executed: %v", err)
	}
}
