package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/providers"
)

// fullADORepositoryAccess is an access report with every required permission
// and Force push held, no bypass and no blanket policy: nothing to report.
func fullADORepositoryAccess() adoRepositoryAccess {
	return adoRepositoryAccess{
		identity: providers.ADOIdentity{ID: "identity-guid", UniqueName: "bot@example.com"},
		permissions: map[providers.ADOGitPermission]bool{
			providers.ADOGitContribute:                true,
			providers.ADOGitPullRequestContribute:     true,
			providers.ADOGitCreateBranch:              true,
			providers.ADOGitForcePush:                 true,
			providers.ADOGitPullRequestPolicyOverride: false,
			providers.ADOGitPolicyExempt:              false,
		},
	}
}

func fullADOBacklogStates() adoBacklogStates {
	return adoBacklogStates{
		createType: "Story",
		createStates: []providers.ADOWorkItemState{
			{Name: "New", Category: "Proposed"},
			{Name: "Closed", Category: "Completed"},
		},
	}
}

// stubADOAccessReads replaces the read-only ADO access and Boards state reads
// so a validate test never leaves the process.
func stubADOAccessReads(t *testing.T, access adoRepositoryAccess, states adoBacklogStates) {
	t.Helper()
	previousAccess := targetADORepositoryAccess
	targetADORepositoryAccess = func(context.Context, instance.RepoRef, credentials.StoreResolver) adoRepositoryAccess {
		return access
	}
	previousStates := targetADOBacklogStates
	targetADOBacklogStates = func(context.Context, instance.RepoRef, string, []string, credentials.StoreResolver) adoBacklogStates {
		return states
	}
	t.Cleanup(func() {
		targetADORepositoryAccess = previousAccess
		targetADOBacklogStates = previousStates
	})
}

func adoAccessTestRepos() []instance.RepoRef {
	return []instance.RepoRef{
		{Provider: "github", Owner: "example-org", Name: "site"},
		{Provider: "ado", Owner: "example-org", Project: "example-project", Name: "web"},
	}
}

func diagnosticCodes(collector *diagnosticCollector) map[string]string {
	codes := map[string]string{}
	for _, d := range collector.findings {
		codes[d.Code] = d.Severity
	}
	return codes
}

func TestCheckADORepositoryAccessReportsIdentityWhenAllHeld(t *testing.T) {
	calls := 0
	previous := targetADORepositoryAccess
	targetADORepositoryAccess = func(ctx context.Context, repo instance.RepoRef, _ credentials.StoreResolver) adoRepositoryAccess {
		calls++
		if repo.Provider != "ado" || repo.Name != "web" {
			t.Fatalf("access read for non-ADO repo %+v", repo)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded access read")
		}
		return fullADORepositoryAccess()
	}
	t.Cleanup(func() { targetADORepositoryAccess = previous })

	var out strings.Builder
	collector := &diagnosticCollector{}
	if !checkADORepositoryAccess(adoAccessTestRepos(), nil, &out, "instance.yaml", collector) {
		t.Fatalf("access check failed:\n%s", out.String())
	}
	if calls != 1 {
		t.Fatalf("access reads = %d, want 1 (the ADO repository only)", calls)
	}
	for _, want := range []string{
		"repos[1] example-org/web: authenticated as ADO identity identity-guid (bot@example.com)",
		"repos[1] example-org/web: required Git permissions held",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if len(collector.findings) != 0 {
		t.Fatalf("diagnostics = %+v, want none", collector.findings)
	}
}

func TestCheckADORepositoryAccessDiagnostics(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*adoRepositoryAccess)
		wantOK   bool
		wantCode string
		wantSev  string
		wantText string
	}{
		{
			name:     "missing create branch is an error",
			mutate:   func(a *adoRepositoryAccess) { a.permissions[providers.ADOGitCreateBranch] = false },
			wantOK:   false,
			wantCode: adoAccessMissingPermissionCode, wantSev: "error", wantText: `lacks "Create branch"`,
		},
		{
			name:     "missing pull request contribute is an error",
			mutate:   func(a *adoRepositoryAccess) { a.permissions[providers.ADOGitPullRequestContribute] = false },
			wantOK:   false,
			wantCode: adoAccessMissingPermissionCode, wantSev: "error", wantText: `lacks "Contribute to pull requests"`,
		},
		{
			name:     "missing force push is a warning",
			mutate:   func(a *adoRepositoryAccess) { a.permissions[providers.ADOGitForcePush] = false },
			wantOK:   true,
			wantCode: adoAccessForcePushCode, wantSev: "warning", wantText: `lacks "Force push"`,
		},
		{
			name:     "held bypass is a warning",
			mutate:   func(a *adoRepositoryAccess) { a.permissions[providers.ADOGitPullRequestPolicyOverride] = true },
			wantOK:   true,
			wantCode: adoAccessBypassCode, wantSev: "warning", wantText: `holds "Bypass policies when completing pull requests"`,
		},
		{
			name:     "held policy exemption is a warning",
			mutate:   func(a *adoRepositoryAccess) { a.permissions[providers.ADOGitPolicyExempt] = true },
			wantOK:   true,
			wantCode: adoAccessBypassCode, wantSev: "warning", wantText: `holds "Bypass policies when pushing"`,
		},
		{
			name: "blanket prefix policy is a warning",
			mutate: func(a *adoRepositoryAccess) {
				a.policies = []providers.ADOBranchPolicy{{ID: 7, TypeName: "Build", RefName: "refs/heads/"}}
			},
			wantOK:   true,
			wantCode: adoAccessBlanketPolicyCode, wantSev: "warning", wantText: `blocking policy 7 (Build) has a Prefix scope "refs/heads/"`,
		},
		{
			name:     "unreadable permissions are unknown, not missing",
			mutate:   func(a *adoRepositoryAccess) { a.permissions, a.permissionsErr = nil, errors.New("status 401") },
			wantOK:   true,
			wantCode: adoAccessUnknownCode, wantSev: "warning", wantText: "permissions are unknown, not missing",
		},
		{
			name:     "unreadable policies are unknown",
			mutate:   func(a *adoRepositoryAccess) { a.policiesErr = errors.New("status 403") },
			wantOK:   true,
			wantCode: adoAccessUnknownCode, wantSev: "warning", wantText: "blanket policies are unknown",
		},
		{
			name:     "unreadable identity is a warning",
			mutate:   func(a *adoRepositoryAccess) { a.identityErr = errors.New("status 401") },
			wantOK:   true,
			wantCode: adoAccessUnknownCode, wantSev: "warning", wantText: "could not read the authenticated identity",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			access := fullADORepositoryAccess()
			tc.mutate(&access)
			stubADOAccessReads(t, access, fullADOBacklogStates())
			var out strings.Builder
			collector := &diagnosticCollector{}
			ok := checkADORepositoryAccess(adoAccessTestRepos(), nil, &out, "instance.yaml", collector)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v:\n%s", ok, tc.wantOK, out.String())
			}
			codes := diagnosticCodes(collector)
			if len(codes) != 1 || codes[tc.wantCode] != tc.wantSev {
				t.Fatalf("diagnostics = %+v, want one %s %s", collector.findings, tc.wantSev, tc.wantCode)
			}
			if !strings.Contains(out.String(), tc.wantText) || !strings.Contains(collector.findings[0].Message, tc.wantText) {
				t.Fatalf("output/diagnostic lack %q:\n%s\n%+v", tc.wantText, out.String(), collector.findings)
			}
			if collector.findings[0].Path != "/repos/1" {
				t.Errorf("diagnostic path = %q, want /repos/1", collector.findings[0].Path)
			}
		})
	}
}

// TestReadADORepositoryAccessIsReadOnly drives the real reads against a
// recorded Azure DevOps: every request goes to the configured organization
// host, and the only non-GET is the permission evaluation.
func TestReadADORepositoryAccessIsReadOnly(t *testing.T) {
	const token = "opaque-test-credential-5741"
	t.Setenv("ADO_ACCESS_TEST_PAT", token)
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	for _, denyEvaluation := range []bool{false, true} {
		var requests []string
		http.DefaultTransport = adoBacklogTestTransport(func(req *http.Request) (*http.Response, error) {
			requests = append(requests, req.Method+" "+req.URL.Path)
			if req.URL.Host != "dev.azure.com" {
				t.Fatalf("request left the configured organization: %s", req.URL)
			}
			if _, password, ok := req.BasicAuth(); !ok || password != token {
				t.Fatal("configured PAT was not used")
			}
			status, body := adoAccessFixture(t, req, token, denyEvaluation)
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})
		access := readADORepositoryAccess(context.Background(), instance.RepoRef{
			Provider: "ado", Owner: "example-org", Project: "example-project", Name: "web",
			Token: instance.TokenRef{Env: "ADO_ACCESS_TEST_PAT"},
		}, nil)
		for _, request := range requests {
			if !strings.HasPrefix(request, http.MethodGet+" ") && request != http.MethodPost+" /example-org/_apis/security/permissionevaluationbatch" {
				t.Fatalf("access check issued a write: %s", request)
			}
		}
		if access.identityErr != nil || access.identity.ID != "identity-guid" {
			t.Fatalf("identity = %+v, %v", access.identity, access.identityErr)
		}
		if access.policiesErr != nil || len(access.policies) != 1 || access.policies[0].ID != 3 {
			t.Fatalf("policies = %+v, %v", access.policies, access.policiesErr)
		}
		if !denyEvaluation {
			if access.permissionsErr != nil || !access.permissions[providers.ADOGitContribute] || access.permissions[providers.ADOGitPolicyExempt] {
				t.Fatalf("permissions = %v, %v", access.permissions, access.permissionsErr)
			}
			continue
		}
		if access.permissionsErr == nil {
			t.Fatal("a refused permission evaluation returned no error")
		}
		if strings.Contains(access.permissionsErr.Error(), token) {
			t.Fatal("permission evaluation error leaked the credential")
		}
	}
}

func adoAccessFixture(t *testing.T, req *http.Request, token string, denyEvaluation bool) (int, string) {
	t.Helper()
	switch req.URL.Path {
	case "/example-org/_apis/connectionData":
		return http.StatusOK, `{"authenticatedUser":{"id":"identity-guid","providerDisplayName":"Bot","properties":{"Account":{"$value":"bot@example.com"}}}}`
	case "/example-org/example-project/_apis/git/repositories/web":
		return http.StatusOK, `{"id":"repo-guid","project":{"id":"project-guid"}}`
	case "/example-org/example-project/_apis/policy/configurations":
		return http.StatusOK, `{"value":[{"id":3,"isEnabled":true,"isBlocking":true,"type":{"displayName":"Build"},"settings":{"scope":[{"repositoryId":"repo-guid","refName":"refs/heads/","matchKind":"Prefix"}]}}]}`
	case "/example-org/_apis/security/permissionevaluationbatch":
		if denyEvaluation {
			return http.StatusForbidden, `{"message":"denied ` + token + `"}`
		}
		var batch struct {
			Evaluations []map[string]interface{} `json:"evaluations"`
		}
		if err := json.NewDecoder(req.Body).Decode(&batch); err != nil {
			t.Fatalf("decode evaluation batch: %v", err)
		}
		for _, evaluation := range batch.Evaluations {
			bit := int(evaluation["permissions"].(float64))
			evaluation["value"] = bit != int(providers.ADOGitPolicyExempt) && bit != int(providers.ADOGitPullRequestPolicyOverride)
		}
		body, err := json.Marshal(batch)
		if err != nil {
			t.Fatal(err)
		}
		return http.StatusOK, string(body)
	default:
		t.Fatalf("unexpected request %s %s", req.Method, req.URL)
		return 0, ""
	}
}

func TestCheckADOBacklogStatesWarnsOnUnknownDoneStates(t *testing.T) {
	states := fullADOBacklogStates()
	states.byType = map[string][]providers.ADOWorkItemState{
		"Bug": {{Name: "Active", Category: "InProgress"}, {Name: "Closed", Category: "Completed"}},
	}
	states.byTypeErrs = map[string]error{"Epic": errors.New("status 404")}
	var gotTypes []string
	previous := targetADOBacklogStates
	targetADOBacklogStates = func(_ context.Context, _ instance.RepoRef, project string, types []string, _ credentials.StoreResolver) adoBacklogStates {
		if project != "work" {
			t.Fatalf("states read in project %q, want the Boards project", project)
		}
		gotTypes = types
		return states
	}
	t.Cleanup(func() { targetADOBacklogStates = previous })

	var out strings.Builder
	collector := &diagnosticCollector{}
	checkADOBacklogStates("alpha", map[string][]string{"Bug": {"closed", "Verified"}, "Epic": {"Done"}},
		instance.RepoRef{Provider: "ado", Owner: "example-org", Name: "web"}, "work", nil, &out, "gaggle.yaml", collector)

	if strings.Join(gotTypes, ",") != "Bug,Epic" {
		t.Fatalf("types read = %v, want [Bug Epic]", gotTypes)
	}
	if !strings.Contains(out.String(), `create type "Story" states: New, Closed`) {
		t.Errorf("output lacks the create type report:\n%s", out.String())
	}
	if len(collector.findings) != 2 {
		t.Fatalf("diagnostics = %+v, want 2 (unknown Verified, unreadable Epic)", collector.findings)
	}
	for i, want := range []string{`names state "Verified"`, `work item type "Epic"`} {
		d := collector.findings[i]
		if d.Code != adoBacklogStatesCode || d.Severity != "warning" || !strings.Contains(d.Message, want) {
			t.Errorf("diagnostic %d = %+v, want a %s warning containing %q", i, d, adoBacklogStatesCode, want)
		}
	}
}

func TestCheckADOBacklogStatesWarnsWhenCreateTypeCannotClose(t *testing.T) {
	states := adoBacklogStates{createType: "Story", createStates: []providers.ADOWorkItemState{{Name: "New", Category: "Proposed"}}}
	stubADOAccessReads(t, fullADORepositoryAccess(), states)
	var out strings.Builder
	collector := &diagnosticCollector{}
	checkADOBacklogStates("alpha", nil, instance.RepoRef{Provider: "ado"}, "work", nil, &out, "gaggle.yaml", collector)
	if len(collector.findings) != 1 || !strings.Contains(collector.findings[0].Message, "no state in the Completed category") {
		t.Fatalf("diagnostics = %+v", collector.findings)
	}
}

func TestValidateCheckReposFailsOnMissingADOPermission(t *testing.T) {
	t.Setenv("GOOBERS_ADO_TOKEN", "")
	root := filepath.Join(t.TempDir(), "ado")
	code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	code, _, stderr = runArgs(t, "connect", "example-org/example-project/web", root)
	if code != 0 {
		t.Fatalf("connect: %d %s", code, stderr)
	}
	t.Setenv("GOOBERS_ADO_TOKEN", "test-pat")
	stubConnectReachability(t, nil)
	previous := targetADOBacklogReachable
	targetADOBacklogReachable = func(context.Context, instance.RepoRef, string, credentials.StoreResolver) error { return nil }
	t.Cleanup(func() { targetADOBacklogReachable = previous })
	access := fullADORepositoryAccess()
	access.permissions[providers.ADOGitContribute] = false
	stubADOAccessReads(t, access, fullADOBacklogStates())

	code, stdout, stderr := runArgs(t, "validate", "--check-repos", "--json", root)
	if code != 1 {
		t.Fatalf("validate: %d stdout=%s stderr=%s", code, stdout, stderr)
	}
	for _, want := range []string{adoAccessMissingPermissionCode, `lacks \"Contribute\"`, "/repos/0", "instance.yaml"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("JSON lacks %q: %s", want, stdout)
		}
	}
}
