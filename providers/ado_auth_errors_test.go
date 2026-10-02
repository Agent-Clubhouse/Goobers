package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const azureCLIFailureCanary = "azure-cli-sensitive-canary"

func TestAzureCLICredentialFailureDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
		hint string
	}{
		{
			name: "missing executable",
			err:  &exec.Error{Name: azureCLIFailureCanary, Err: exec.ErrNotFound},
			want: "executable lookup failed",
			hint: "PATH",
		},
		{
			name: "untrusted current directory",
			err:  &exec.Error{Name: azureCLIFailureCanary, Err: exec.ErrDot},
			want: "executable lookup failed",
			hint: "trusted Azure CLI",
		},
		{
			name: "start failure",
			err:  &os.PathError{Op: "fork/exec", Path: azureCLIFailureCanary, Err: os.ErrPermission},
			want: "process could not start",
			hint: "permissions",
		},
		{
			name: "unclassified I/O is not a start failure",
			err:  &os.PathError{Op: "read", Path: azureCLIFailureCanary, Err: os.ErrPermission},
			want: "command failed",
			hint: "--query expiresOn --output tsv",
		},
		{
			name: "exit without available status",
			err:  &exec.ExitError{Stderr: []byte(azureCLIFailureCanary)},
			want: "process exited unsuccessfully",
			hint: "--query expiresOn --output tsv",
		},
		{
			name: "canceled",
			err:  fmt.Errorf("%s: %w", azureCLIFailureCanary, context.Canceled),
			want: "command was canceled",
			hint: "retry",
		},
		{
			name: "deadline",
			err:  fmt.Errorf("%s: %w", azureCLIFailureCanary, context.DeadlineExceeded),
			want: "command timed out",
			hint: "network",
		},
		{
			name: "untyped error is not diagnosed from its text",
			err:  errors.New("exit status 1: signed out: " + azureCLIFailureCanary),
			want: "command failed",
			hint: "only if",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &adoAuthRunner{
				out: []byte(`{"accessToken":"` + azureCLIFailureCanary + `"}`),
				err: tc.err,
			}
			credential, err := NewAzureCLIADOCredentialSource(runner, azureCLIFailureCanary).Credential(context.Background())
			if err == nil {
				t.Fatal("failed Azure CLI command was accepted")
			}
			if credential.Secret != "" {
				t.Fatal("failed command returned a credential")
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("original error identity was lost: %v", err)
			}
			for _, want := range []string{"azure CLI get-access-token:", tc.want, tc.hint} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			wrapped := fmt.Errorf("resolve ADO repository credential: %w", err)
			for _, rendered := range []string{err.Error(), fmt.Sprintf("%v", wrapped), fmt.Sprintf("%+v", wrapped)} {
				if strings.Contains(rendered, azureCLIFailureCanary) || strings.Contains(rendered, "signed out") {
					t.Fatalf("unsafe or unproven diagnostic: %s", rendered)
				}
			}
		})
	}
}

type azureCLIFailureRunnerFunc func(context.Context, string, ...string) ([]byte, error)

func (f azureCLIFailureRunnerFunc) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return f(ctx, name, args...)
}

func TestAzureCLICredentialFailurePreservesCancellationDuringCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exit := &exec.ExitError{Stderr: []byte(azureCLIFailureCanary)}
	runner := azureCLIFailureRunnerFunc(func(context.Context, string, ...string) ([]byte, error) {
		cancel()
		return []byte(azureCLIFailureCanary), exit
	})
	_, err := NewAzureCLIADOCredentialSource(runner, "").Credential(ctx)
	if !errors.Is(err, context.Canceled) || !errors.Is(err, exit) {
		t.Fatalf("error = %v, want both cancellation and the process error", err)
	}
	if !strings.Contains(err.Error(), "command was canceled") || strings.Contains(err.Error(), "exited") {
		t.Fatalf("cancellation was misclassified: %v", err)
	}
}

func TestAzureCLICredentialFailurePrefersDeadlineOverProcessExit(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	exit := &exec.ExitError{Stderr: []byte(azureCLIFailureCanary)}
	err := azureCLICommandError(ctx, exit, []byte(azureCLIExpiredLoginOutput))
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, exit) {
		t.Fatalf("error = %v, want both deadline and the process error", err)
	}
	if !strings.Contains(err.Error(), "command timed out") || strings.Contains(err.Error(), "exited") || strings.Contains(err.Error(), "sign-in") {
		t.Fatalf("deadline was misclassified: %v", err)
	}
}

func TestAzureCLICredentialFailureStopsRepositoryProbe(t *testing.T) {
	runner := &adoAuthRunner{err: errors.New(azureCLIFailureCanary)}
	gitRunner := &adoAuthRunner{}
	provider := NewADOProvider("example-org", "example-project", "",
		WithADOCredentialSource(NewAzureCLIADOCredentialSource(runner, "")),
		func(p *ADOProvider) { p.Runner = gitRunner },
	)
	err := provider.RepositoryReachable(context.Background(), RepositoryRef{Name: "example-repo"})
	if err == nil || gitRunner.name != "" {
		t.Fatalf("repository probe must stop before git on credential failure: %v, git command %q", err, gitRunner.name)
	}
	if !strings.Contains(err.Error(), "resolve ADO repository credential: resolve ADO credential: azure CLI get-access-token: command failed") {
		t.Fatalf("error = %v, want classified credential failure", err)
	}
	if strings.Contains(err.Error(), azureCLIFailureCanary) {
		t.Fatalf("repository diagnostic exposed provider output: %v", err)
	}
}

// Representative failed `az account get-access-token` output. Each carries the
// canary so the no-echo assertions prove no substring of it is rendered.
const (
	azureCLIExpiredLoginOutput = "ERROR: AADSTS700082: The refresh token has expired due to inactivity. " +
		"Trace ID: " + azureCLIFailureCanary + "\n" +
		"Interactive authentication is needed. Please run:\naz login --scope " + AzureDevOpsResourceID + "/.default\n"
	azureCLIOfflineOutput = "ERROR: HTTPSConnectionPool(host='login.microsoftonline.com', port=443): Max retries exceeded " +
		"with url: /" + azureCLIFailureCanary + "/oauth2/v2.0/token (Caused by NewConnectionError(" +
		"'<urllib3.connection.HTTPSConnection object>: Failed to establish a new connection: " +
		"[Errno 8] nodename nor servname provided, or not known'))\n"
	azureCLINoAccountOutput = "ERROR: Please run 'az login' to setup account. " + azureCLIFailureCanary + "\n"
)

// azureCLIOutputFragments are distinctive pieces of the sample outputs; none
// may appear in a rendered diagnostic.
var azureCLIOutputFragments = []string{
	azureCLIFailureCanary, "AADSTS", "Trace ID", "inactivity", "HTTPSConnectionPool", "login.microsoftonline.com",
	"Errno", "nodename", "NewConnectionError", "Max retries", "to setup account", "--scope", "ERROR:",
}

func assertAzureCLIDiagnosticWithholdsOutput(t *testing.T, err error) {
	t.Helper()
	wrapped := fmt.Errorf("resolve ADO Git credential: %w", err)
	for _, rendered := range []string{err.Error(), fmt.Sprintf("%v", wrapped), fmt.Sprintf("%+v", wrapped)} {
		for _, fragment := range azureCLIOutputFragments {
			if strings.Contains(rendered, fragment) {
				t.Fatalf("diagnostic echoed Azure CLI output fragment %q: %s", fragment, rendered)
			}
		}
	}
}

func TestAzureCLICredentialFailureClassifiesOutputMarkers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		out    string
		code   string
		detail string
		hint   string
	}{
		{"expired refresh token", azureCLIExpiredLoginOutput, azureCLISignInRequiredCode, "sign-in expired or requires interaction", "run az login"},
		{"sign-in frequency", "AADSTS70043: " + azureCLIFailureCanary, azureCLISignInRequiredCode, "sign-in expired", "run az login"},
		{"grant expired after password change", "aadsts50173 " + azureCLIFailureCanary, azureCLISignInRequiredCode, "sign-in expired", "run az login"},
		{"interaction required", `{"error":"interaction_required","x":"` + azureCLIFailureCanary + `"}`, azureCLISignInRequiredCode, "sign-in expired", "run az login"},
		{"generic re-login advice", "To re-authenticate, please run: az login " + azureCLIFailureCanary, azureCLISignInRequiredCode, "sign-in expired", "run az login"},
		{"offline (macOS resolver)", azureCLIOfflineOutput, azureCLINetworkUnreachableCode, "could not reach the network", "network connectivity"},
		{"offline (Linux resolver)", "[Errno -3] Temporary failure in name resolution " + azureCLIFailureCanary, azureCLINetworkUnreachableCode, "could not reach the network", "DNS"},
		{"offline (Windows resolver)", "[Errno 11001] getaddrinfo failed " + azureCLIFailureCanary, azureCLINetworkUnreachableCode, "could not reach the network", "DNS"},
		{"network unreachable", "[Errno 51] Network is unreachable " + azureCLIFailureCanary, azureCLINetworkUnreachableCode, "could not reach the network", "signing in again does not fix this"},
		{"offline with appended az login advice", azureCLIOfflineOutput + "Please run 'az login' to setup account.", azureCLINetworkUnreachableCode, "could not reach the network", "network connectivity"},
		{"server sign-in error beats network wording", azureCLIExpiredLoginOutput + "Max retries exceeded", azureCLISignInRequiredCode, "sign-in expired", "run az login"},
		{"wrong tenant (AADSTS50020)", "ERROR: AADSTS50020: User account from identity provider does not exist in tenant. Trace ID: " + azureCLIFailureCanary + "\nPlease run az login", azureCLIWrongTenantCode, "different tenant", "az login --tenant <tenant>"},
		{"wrong tenant (tenant not found)", "aadsts90002: tenant not found " + azureCLIFailureCanary, azureCLIWrongTenantCode, "different tenant", "az login --tenant <tenant>"},
		{"wrong tenant (resource not in tenant)", "AADSTS500011 " + azureCLIFailureCanary, azureCLIWrongTenantCode, "different tenant", "az login --tenant <tenant>"},
		{"wrong tenant beats generic az login advice", "AADSTS90072 az login " + azureCLIFailureCanary, azureCLIWrongTenantCode, "different tenant", "az login --tenant <tenant>"},
		{"wrong tenant beats network wording", "AADSTS50020 Max retries exceeded " + azureCLIFailureCanary, azureCLIWrongTenantCode, "different tenant", "az login --tenant <tenant>"},
		{"sign-in error beats tenant code", "AADSTS700082 AADSTS50020 " + azureCLIFailureCanary, azureCLISignInRequiredCode, "sign-in expired", "run az login"},
		{"no account", azureCLINoAccountOutput, azureCLINoAccountCode, "no Azure CLI account is signed in", "run az login"},
		{"unrecognized output", `{"accessToken":"` + azureCLIFailureCanary + `"}`, azureCLIExitCode, "process exited unsuccessfully", "only if that check requests sign-in"},
		{"no output", "", azureCLIExitCode, "process exited unsuccessfully", "only if that check requests sign-in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exit := &exec.ExitError{}
			runner := &adoAuthRunner{out: []byte(tc.out), err: exit}
			source := NewAzureCLIADOCredentialSource(runner, "")
			credential, err := source.Credential(context.Background())
			if err == nil || credential.Secret != "" {
				t.Fatalf("failed Azure CLI command was accepted: %v", err)
			}
			if !errors.Is(err, exit) {
				t.Fatalf("process error identity was lost: %v", err)
			}
			var failure *azureCLICommandFailure
			if !errors.As(err, &failure) || failure.ErrorCode() != tc.code {
				t.Fatalf("error = %v, want code %q", err, tc.code)
			}
			for _, want := range []string{"azure CLI get-access-token: ", tc.detail, tc.hint, "process exited"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			assertAzureCLIDiagnosticWithholdsOutput(t, err)

			// A classified failure is not cached: the next fetch runs az again.
			runner.out = []byte(`{"accessToken":"next-token","expires_on":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`)
			runner.err = nil
			if next, err := source.Credential(context.Background()); err != nil || next.Secret != "next-token" || runner.calls != 2 {
				t.Fatalf("recovery fetch failed after %d calls: %v", runner.calls, err)
			}
		})
	}
}

func TestAzureCLICredentialFailureReadsOutputOnlyForAProcessExit(t *testing.T) {
	out := []byte(azureCLIExpiredLoginOutput + azureCLIOfflineOutput)
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"untyped runner error", errors.New(azureCLIFailureCanary), "command failed"},
		{"lookup failure", &exec.Error{Name: "az", Err: exec.ErrNotFound}, "executable lookup failed"},
		{"start failure", &os.PathError{Op: "fork/exec", Path: "az", Err: os.ErrPermission}, "process could not start"},
		{"canceled", context.Canceled, "command was canceled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := azureCLICommandError(context.Background(), tc.err, out)
			if !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "sign-in expired") || strings.Contains(err.Error(), "could not reach") {
				t.Fatalf("error = %v, want only %q", err, tc.want)
			}
			assertAzureCLIDiagnosticWithholdsOutput(t, err)
		})
	}
}

func TestAzureCLICredentialFailureReportsAmbiguousPathWithoutProbingAlternatives(t *testing.T) {
	exit := &exec.ExitError{Stderr: []byte(azureCLIFailureCanary)}
	err := azureCLICommandError(context.Background(), &azureCLIPathAmbiguityError{
		cause:          exit,
		candidateCount: 3,
	}, []byte(`{"accessToken":"`+azureCLIFailureCanary+`"}`))
	var failure *azureCLICommandFailure
	if !errors.As(err, &failure) || failure.ErrorCode() != azureCLIPathAmbiguousCode {
		t.Fatalf("error = %v, want code %q", err, azureCLIPathAmbiguousCode)
	}
	for _, want := range []string{"process exited unsuccessfully", "found 3 Azure CLI launchers", "used the first", "remove or reorder"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	assertAzureCLIDiagnosticWithholdsOutput(t, err)
}

func TestAzureCLICredentialFailureReportsAmbiguousPathAlongsideTimeoutAndCancel(t *testing.T) {
	for name, tc := range map[string]struct {
		cause      error
		wantCode   string
		wantDetail string
	}{
		"timeout":  {context.DeadlineExceeded, azureCLITimeoutCode, "command timed out"},
		"canceled": {context.Canceled, azureCLICanceledCode, "command was canceled"},
	} {
		t.Run(name, func(t *testing.T) {
			err := azureCLICommandError(context.Background(), &azureCLIPathAmbiguityError{
				cause:          tc.cause,
				candidateCount: 2,
			}, []byte(azureCLIFailureCanary))
			var failure *azureCLICommandFailure
			if !errors.As(err, &failure) || failure.ErrorCode() != tc.wantCode {
				t.Fatalf("error = %v, want code %q", err, tc.wantCode)
			}
			for _, want := range []string{tc.wantDetail, "found 2 Azure CLI launchers", "used the first"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			assertAzureCLIDiagnosticWithholdsOutput(t, err)
		})
	}
}

func TestAzureCLICredentialFailureKeepsKnownCauseAndReportsAmbiguousPath(t *testing.T) {
	exit := &exec.ExitError{}
	err := azureCLICommandError(context.Background(), &azureCLIPathAmbiguityError{
		cause:          exit,
		candidateCount: 2,
	}, []byte(azureCLIExpiredLoginOutput))
	var failure *azureCLICommandFailure
	if !errors.As(err, &failure) || failure.ErrorCode() != azureCLISignInRequiredCode {
		t.Fatalf("error = %v, want code %q", err, azureCLISignInRequiredCode)
	}
	if !strings.Contains(err.Error(), "sign-in expired or requires interaction") {
		t.Errorf("error %q does not contain the classified cause", err)
	}
	for _, want := range []string{"found 2 Azure CLI launchers", "used the first", "remove or reorder"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not report ambiguity detail %q", err, want)
		}
	}
	assertAzureCLIDiagnosticWithholdsOutput(t, err)
}

func TestAzureCLICredentialFailureStartFailureMatchesOnlyForkExec(t *testing.T) {
	// os.StartProcess reports Op "fork/exec" on every platform, Windows
	// included; any other PathError op is not a start failure.
	for op, want := range map[string]string{
		"fork/exec":     "process could not start",
		"CreateProcess": "command failed",
	} {
		err := azureCLICommandError(context.Background(), &os.PathError{Op: op, Path: "az", Err: os.ErrPermission}, nil)
		if !strings.Contains(err.Error(), want) {
			t.Errorf("op %q: error = %v, want %q", op, err, want)
		}
	}
}

func TestAzureCLICredentialFailureSurfacesThroughGitAuthEnvironment(t *testing.T) {
	for out, want := range map[string]string{
		azureCLIExpiredLoginOutput: "resolve ADO Git credential: azure CLI get-access-token: Azure CLI sign-in expired or requires interaction (process exited",
		azureCLIOfflineOutput:      "resolve ADO Git credential: azure CLI get-access-token: Azure CLI could not reach the network (process exited",
	} {
		runner := &adoAuthRunner{out: []byte(out), err: &exec.ExitError{}}
		remote := "https://dev.azure.com/example-org/example-project/" + "_git/example-repo"
		_, err := ADOGitAuthEnvironment(context.Background(), NewAzureCLIADOCredentialSource(runner, ""), nil, remote)
		if err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatalf("error = %v, want prefix %q", err, want)
		}
		assertAzureCLIDiagnosticWithholdsOutput(t, err)
	}
}

func TestAzureCLIWrongTenantMarkersAllClassify(t *testing.T) {
	for _, marker := range azureCLIWrongTenantMarkers {
		class, ok := classifyAzureCLIOutput([]byte("ERROR: " + strings.ToUpper(string(marker)) + ": detail"))
		if !ok || class.code != azureCLIWrongTenantCode {
			t.Errorf("marker %q classified as %q (ok=%v), want %q", marker, class.code, ok, azureCLIWrongTenantCode)
		}
	}
}
