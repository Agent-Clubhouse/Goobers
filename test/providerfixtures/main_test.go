package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/providerfixture"
)

func TestRefreshRequiresDedicatedCredential(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	exitCode := run(
		[]string{"refresh", "-repository", "acme/fixtures", "-issue", "7", "-output", "candidate.json"},
		func(string) string { return "" },
		&stdout,
		&stderr,
	)
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !bytes.Contains(stderr.Bytes(), []byte(tokenEnvironment+" is required")) {
		t.Fatalf("missing prerequisite was not reported clearly: %s", stderr.String())
	}
}

func TestRefreshWritesNormalizedCandidate(t *testing.T) {
	baselinePath := filepath.Join("..", "providers", "testdata", "github_contract.json")
	fixture, err := providerfixture.Read(baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "candidate.json")
	var stdout, stderr bytes.Buffer
	exitCode := runWithRefreshers(
		[]string{"refresh", "-repository", "acme/fixtures", "-issue", "7", "-output", output},
		func(name string) string {
			if name == tokenEnvironment {
				return "dedicated-token"
			}
			return ""
		},
		&stdout,
		&stderr,
		func(_ context.Context, cfg providerfixture.RefreshConfig) (providerfixture.Fixture, error) {
			if cfg.Repository != (providerfixture.Repository{Owner: "acme", Name: "fixtures"}) ||
				cfg.Issue != "7" || cfg.Token != "dedicated-token" {
				t.Fatalf("refresh config = %+v", cfg)
			}
			return fixture, nil
		},
		adoRefreshers{},
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %s", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), output) {
		t.Fatalf("stdout does not identify candidate: %s", stdout.String())
	}
	if _, err := providerfixture.Read(output); err != nil {
		t.Fatalf("read written candidate: %v", err)
	}
}

func TestRefreshAcceptsPullRequestTarget(t *testing.T) {
	baselinePath := filepath.Join("..", "providers", "testdata", "github_pr_contract.json")
	fixture, err := providerfixture.Read(baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "candidate.json")
	var stdout, stderr bytes.Buffer
	exitCode := runWithRefreshers(
		[]string{"refresh", "-repository", "acme/fixtures", "-pull-request", "8", "-output", output},
		func(name string) string {
			if name == tokenEnvironment {
				return "dedicated-token"
			}
			return ""
		},
		&stdout,
		&stderr,
		func(_ context.Context, cfg providerfixture.RefreshConfig) (providerfixture.Fixture, error) {
			if cfg.Repository != (providerfixture.Repository{Owner: "acme", Name: "fixtures"}) ||
				cfg.PullRequest != "8" || cfg.Issue != "" || cfg.Token != "dedicated-token" {
				t.Fatalf("refresh config = %+v", cfg)
			}
			return fixture, nil
		},
		adoRefreshers{},
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %s", exitCode, stderr.String())
	}
	if _, err := providerfixture.Read(output); err != nil {
		t.Fatalf("read written candidate: %v", err)
	}
}

func TestADORefreshUsesProvisionedPATAndWritesCandidate(t *testing.T) {
	baselinePath := filepath.Join("..", "providers", "testdata", "ado_contract.json")
	fixture, err := providerfixture.Read(baselinePath)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "candidate.json")
	var stdout, stderr bytes.Buffer
	exitCode := runWithRefreshers(
		[]string{
			"refresh", "-provider", "ado",
			"-organization-url", "https://dev.azure.com/acme",
			"-project", "widgets",
			"-work-item", "7",
			"-output", output,
		},
		func(name string) string {
			if name == adoTokenEnvironment {
				return "ado-pat"
			}
			return ""
		},
		&stdout,
		&stderr,
		providerfixture.Refresh,
		adoRefreshers{refresh: func(_ context.Context, cfg providerfixture.ADORefreshConfig) (providerfixture.Fixture, error) {
			if cfg.OrganizationURL != "https://dev.azure.com/acme" ||
				cfg.Project != "widgets" || cfg.WorkItem != "7" || cfg.Token != "ado-pat" {
				t.Fatalf("ADO refresh config = %+v", cfg)
			}
			return fixture, nil
		}},
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %s", exitCode, stderr.String())
	}
	if _, err := providerfixture.Read(output); err != nil {
		t.Fatalf("read written ADO candidate: %v", err)
	}
}

// TestADORefreshResolvesTheWorkflowManagedFixture covers the workflow's path:
// with no pinned -work-item, refresh reads whichever open fixture the resolver
// returns (here a recreated one), and a missing fixture without
// -provision-fixture fails with the recovery to run.
func TestADORefreshResolvesTheWorkflowManagedFixture(t *testing.T) {
	t.Parallel()
	baseline, err := providerfixture.Read(filepath.Join("..", "providers", "testdata", "ado_contract.json"))
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(name string) string {
		if name == adoTokenEnvironment {
			return "ado-pat"
		}
		return ""
	}
	args := func(output string, extra ...string) []string {
		return append([]string{
			"refresh", "-provider", "ado",
			"-organization-url", "https://dev.azure.com/acme",
			"-project", "widgets",
			"-output", output,
		}, extra...)
	}
	missing := func(_ context.Context, cfg providerfixture.ADOFixtureConfig) (providerfixture.ADOFixtureResolution, error) {
		if !cfg.Provision {
			return providerfixture.ADOFixtureResolution{}, fmt.Errorf("%w: pass -provision-fixture", providerfixture.ErrADOFixtureMissing)
		}
		if cfg.WorkItemType != providerfixture.ADOFixtureDefaultType || cfg.Token != "ado-pat" || cfg.Project != "widgets" {
			t.Errorf("ensure config = %+v", cfg)
		}
		return providerfixture.ADOFixtureResolution{WorkItem: "1812", Created: true}, nil
	}
	refreshed := ""
	ado := adoRefreshers{
		ensure: missing,
		refresh: func(_ context.Context, cfg providerfixture.ADORefreshConfig) (providerfixture.Fixture, error) {
			refreshed = cfg.WorkItem
			return baseline, nil
		},
	}

	var stdout, stderr bytes.Buffer
	output := filepath.Join(t.TempDir(), "candidate.json")
	if code := runWithRefreshers(args(output), getenv, &stdout, &stderr, providerfixture.Refresh, ado); code != 1 {
		t.Fatalf("missing fixture without -provision-fixture exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "-provision-fixture") || refreshed != "" {
		t.Fatalf("missing fixture: stderr %q, refreshed %q; want recovery hint and no refresh", stderr.String(), refreshed)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runWithRefreshers(args(output, "-provision-fixture"), getenv, &stdout, &stderr, providerfixture.Refresh, ado); code != 0 {
		t.Fatalf("provisioned refresh exit = %d, stderr %s", code, stderr.String())
	}
	if refreshed != "1812" || !strings.Contains(stdout.String(), "#1812: created") {
		t.Fatalf("refreshed %q, stdout %q; want the recreated fixture", refreshed, stdout.String())
	}

	stderr.Reset()
	if code := runWithRefreshers(args(output, "-provision-fixture", "-work-item", "7"), getenv, &stdout, &stderr, providerfixture.Refresh, ado); code != 1 {
		t.Fatalf("-provision-fixture with -work-item exit = %d, want 1", code)
	}
}

func TestADORefreshRequiresPAT(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	exitCode := run(
		[]string{
			"refresh", "-provider", "ado",
			"-organization-url", "https://dev.azure.com/acme",
			"-project", "widgets",
			"-work-item", "7",
			"-output", "candidate.json",
		},
		func(string) string { return "" },
		&stdout,
		&stderr,
	)
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), adoTokenEnvironment+" is required") {
		t.Fatalf("missing ADO PAT was not reported clearly: %s", stderr.String())
	}
}

func TestContractAndDriftCommands(t *testing.T) {
	t.Parallel()
	baseline := filepath.Join("..", "providers", "testdata", "github_contract.json")
	var stdout, stderr bytes.Buffer
	if got := run(
		[]string{"contract", "-fixture", baseline},
		func(string) string { return "" },
		&stdout,
		&stderr,
	); got != 0 {
		t.Fatalf("contract exit code = %d, stderr = %s", got, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if got := run(
		[]string{"drift", "-baseline", baseline, "-candidate", baseline},
		func(string) string { return "" },
		&stdout,
		&stderr,
	); got != 0 {
		t.Fatalf("no-drift exit code = %d, stderr = %s", got, stderr.String())
	}

	raw, err := os.ReadFile(baseline)
	if err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(t.TempDir(), "candidate.json")
	materiallyChanged := strings.Replace(string(raw), "Stable fixture body.", "Changed fixture body.", 1)
	if err := os.WriteFile(candidate, []byte(materiallyChanged), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if got := run(
		[]string{"drift", "-baseline", baseline, "-candidate", candidate},
		func(string) string { return "" },
		&stdout,
		&stderr,
	); got != 3 {
		t.Fatalf("drift exit code = %d, want 3; stderr = %s", got, stderr.String())
	}
}

func TestContractCommandUsesDistinctFailure(t *testing.T) {
	t.Parallel()
	baseline := filepath.Join("..", "providers", "testdata", "github_contract.json")
	raw, err := os.ReadFile(baseline)
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(t.TempDir(), "broken.json")
	withoutTitles := strings.ReplaceAll(string(raw), `"title": "Stable fixture issue"`, `"title": ""`)
	if err := os.WriteFile(broken, []byte(withoutTitles), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if got := run(
		[]string{"contract", "-fixture", broken},
		func(string) string { return "" },
		&stdout,
		&stderr,
	); got != 2 {
		t.Fatalf("contract failure exit code = %d, want 2; stderr = %s", got, stderr.String())
	}
}

func TestOutcomeExitCodesAreDistinct(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want int
	}{
		{err: providerfixture.ErrContractAssertion, want: 2},
		{err: providerfixture.ErrFixtureDrift, want: 3},
	}
	for _, tc := range cases {
		if got := exitCode(tc.err); got != tc.want {
			t.Errorf("error %v exit code = %d, want %d", tc.err, got, tc.want)
		}
	}
}
