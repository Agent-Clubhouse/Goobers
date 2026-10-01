package providers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	err := azureCLICommandError(ctx, exit)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, exit) {
		t.Fatalf("error = %v, want both deadline and the process error", err)
	}
	if !strings.Contains(err.Error(), "command timed out") || strings.Contains(err.Error(), "exited") {
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
