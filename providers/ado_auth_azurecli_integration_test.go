//go:build integration

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

// TestIntegrationAzureCLILauncher runs NewAzureCLIADOCredentialSource against
// a synthetic Azure CLI (testdata/azurecli) behind the platform's real
// launcher shape: the MSI's az.cmd on Windows, the packaged shell wrapper
// elsewhere. Both run only the synthetic executable, never Azure CLI.
func TestIntegrationAzureCLILauncher(t *testing.T) {
	testdep.Require(t, "go")

	root := filepath.Join(t.TempDir(), "Program Files (x86)", "Azure CLI")
	bin := filepath.Join(root, "wbin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(root, "python")
	if runtime.GOOS == "windows" {
		testdep.Require(t, "cmd.exe")
		python += ".exe"
	} else {
		testdep.Require(t, "sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", python, filepath.Join("testdata", "azurecli", "main.go"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build synthetic Azure CLI fixture: %v\n%s", err, output)
	}
	if runtime.GOOS == "windows" {
		// Match the public MSI launcher's sibling-Python lookup and argument
		// forwarding.
		script := "@IF EXIST \"%~dp0\\..\\python.exe\" (\r\n" +
			"  SET AZ_INSTALLER=MSI\r\n" +
			"  \"%~dp0\\..\\python.exe\" -IBm azure.cli %*\r\n" +
			") ELSE (\r\n" +
			"  exit /b 1\r\n" +
			")\r\n"
		if err := os.WriteFile(filepath.Join(bin, "az.cmd"), []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	} else {
		// Match the packaged POSIX wrapper, which execs the bundled Python
		// with -m azure.cli and forwards every argument.
		script := "#!/bin/sh\nexec \"$(dirname \"$0\")/../python\" -IBm azure.cli \"$@\"\n"
		if err := os.WriteFile(filepath.Join(bin, "az"), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	runAzureCLIFixtureCases(t, filepath.Join(root, "arguments.json"))
}

// azureCLIFixtureLeaks are fragments of the fixture's failure output; none may
// reach a rendered diagnostic.
var azureCLIFixtureLeaks = []string{
	"fixture-sensitive", "AADSTS", "Trace ID", "inactivity", "HTTPSConnectionPool", "login.microsoftonline.com",
	"Errno", "nodename", "NewConnectionError", "Max retries", "to setup account", "--scope", "ERROR:",
}

// runAzureCLIFixtureCases drives NewAzureCLIADOCredentialSource against the
// synthetic Azure CLI already placed first on PATH behind a platform launcher.
func runAzureCLIFixtureCases(t *testing.T, argsFile string) {
	t.Setenv("GOOBERS_AZURE_CLI_FIXTURE_ARGS", argsFile)
	for _, tc := range []struct {
		name     string
		mode     string
		tenant   string
		exitCode int
		want     []string
	}{
		{name: "ambient tenant"},
		{name: "explicit tenant", tenant: "tenant.example.test"},
		{
			name: "command failure", mode: "failure", exitCode: 7,
			want: []string{"get-access-token: process exited with code 7;", "--query expiresOn --output tsv", "same user/process context", "only if"},
		},
		{
			name: "sign-in expired", mode: "login", exitCode: 1,
			want: []string{"get-access-token: Azure CLI sign-in expired or requires interaction (process exited with code 1); run az login", "--tenant", "same user/process context"},
		},
		{
			name: "no account", mode: "account", exitCode: 1,
			want: []string{"get-access-token: no Azure CLI account is signed in (process exited with code 1); run az login"},
		},
		{
			name: "offline host", mode: "offline", exitCode: 1,
			want: []string{"get-access-token: Azure CLI could not reach the network (process exited with code 1); check network connectivity", "signing in again does not fix this"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOOBERS_AZURE_CLI_FIXTURE_MODE", tc.mode)
			source := NewAzureCLIADOCredentialSource(nil, tc.tenant)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			credential, err := source.Credential(ctx)
			if tc.exitCode == 0 {
				if err != nil {
					t.Fatalf("synthetic Azure CLI launcher failed: %v", err)
				}
				if credential.Kind != ADOCredentialKindBearer || credential.Secret != "fixture-success-token" || !credential.ExpiresAt.After(time.Now()) {
					t.Fatal("synthetic Azure CLI launcher did not return the expected expiring bearer")
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != tc.exitCode {
					t.Fatalf("error = %v, want typed exit code %d", err, tc.exitCode)
				}
				for _, want := range tc.want {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				if credential.Secret != "" {
					t.Fatal("failed command returned a credential")
				}
				wrapped := fmt.Errorf("resolve ADO Git credential: %w", err)
				for _, leak := range azureCLIFixtureLeaks {
					if strings.Contains(fmt.Sprintf("%+v", wrapped), leak) {
						t.Fatalf("failed command echoed Azure CLI output fragment %q: %v", leak, err)
					}
				}
				t.Setenv("GOOBERS_AZURE_CLI_FIXTURE_MODE", "")
				if _, err := source.Credential(ctx); err != nil {
					t.Fatalf("a failed credential fetch was cached: %v", err)
				}
			}

			data, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			want := []string{"-IBm", "azure.cli", "account", "get-access-token", "--resource", AzureDevOpsResourceID, "--output", "json"}
			if tc.tenant != "" {
				want = append(want, "--tenant", tc.tenant)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("fixture arguments = %q, want %q", got, want)
			}
		})
	}
}
