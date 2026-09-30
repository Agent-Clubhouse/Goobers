//go:build integration && windows

package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/test/testsupport/testdep"
)

func TestIntegrationAzureCLIWindowsMSILauncher(t *testing.T) {
	testdep.Require(t, "cmd.exe", "go")

	root := filepath.Join(t.TempDir(), "Program Files (x86)", "Azure CLI")
	bin := filepath.Join(root, "wbin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(root, "python.exe"), filepath.Join("testdata", "azurecli", "main.go"))
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build synthetic Azure CLI fixture: %v\n%s", err, output)
	}
	// Match the public MSI launcher's sibling-Python lookup and argument
	// forwarding, but run only our synthetic executable, never Azure CLI.
	script := "@IF EXIST \"%~dp0\\..\\python.exe\" (\r\n" +
		"  SET AZ_INSTALLER=MSI\r\n" +
		"  \"%~dp0\\..\\python.exe\" -IBm azure.cli %*\r\n" +
		") ELSE (\r\n" +
		"  exit /b 1\r\n" +
		")\r\n"
	if err := os.WriteFile(filepath.Join(bin, "az.cmd"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PATHEXT", ".COM;.EXE;.BAT;.CMD")
	argsFile := filepath.Join(root, "arguments.json")
	t.Setenv("GOOBERS_AZURE_CLI_FIXTURE_ARGS", argsFile)

	for _, tc := range []struct {
		name     string
		mode     string
		tenant   string
		exitCode int
	}{
		{name: "ambient tenant"},
		{name: "explicit tenant", tenant: "tenant.example.test"},
		{name: "command failure", mode: "failure", exitCode: 7},
		{name: "login required", mode: "login", exitCode: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOOBERS_AZURE_CLI_FIXTURE_MODE", tc.mode)
			source := NewAzureCLIADOCredentialSource(nil, tc.tenant)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			credential, err := source.Credential(ctx)
			if tc.exitCode == 0 {
				if err != nil {
					t.Fatalf("synthetic MSI launcher failed: %v", err)
				}
				if credential.Kind != ADOCredentialKindBearer || credential.Secret != "fixture-success-token" || !credential.ExpiresAt.After(time.Now()) {
					t.Fatal("synthetic MSI launcher did not return the expected expiring bearer")
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != tc.exitCode {
					t.Fatalf("error = %v, want typed exit code %d", err, tc.exitCode)
				}
				for _, want := range []string{
					fmt.Sprintf("process exited with code %d", tc.exitCode),
					"--query expiresOn --output tsv",
					"same user/process context",
					"only if",
				} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not contain %q", err, want)
					}
				}
				if credential.Secret != "" || strings.Contains(err.Error(), "fixture-sensitive") || strings.Contains(err.Error(), "signed out") {
					t.Fatalf("failed command leaked a credential or claimed an unproven cause: %v", err)
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
				t.Fatalf("MSI fixture arguments = %q, want %q", got, want)
			}
		})
	}
}
