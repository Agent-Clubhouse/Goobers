package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/instance"
)

type adoBacklogTestTransport func(*http.Request) (*http.Response, error)

func (f adoBacklogTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestADOBacklogProbeUsesBoardsEndpointAndScrubsPAT(t *testing.T) {
	const token = "opaque-test-credential-2727"
	t.Setenv("ADO_BACKLOG_TEST_PAT", token)
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized} {
		calls := 0
		http.DefaultTransport = adoBacklogTestTransport(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.URL.Host != "dev.azure.com" || req.URL.Path != "/org/work/_apis/wit/wiql" || req.URL.Query().Get("$top") != "1" || req.Method != http.MethodPost {
				t.Fatalf("incorrect Boards probe: %s %s", req.Method, req.URL)
			}
			_, password, ok := req.BasicAuth()
			if !ok || password != token {
				t.Fatal("configured PAT was not used")
			}
			body := `{"workItems":[]}`
			if status != http.StatusOK {
				body = `{"message":"denied ` + token + `"}`
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		})
		err := adoBacklogReachable(context.Background(), instance.RepoRef{Provider: "ado", Owner: "org", Project: "code", Name: "web", Token: instance.TokenRef{Env: "ADO_BACKLOG_TEST_PAT"}}, "work", nil)
		if calls != 1 || (status == http.StatusOK) != (err == nil) {
			t.Fatalf("status=%d calls=%d error=%v", status, calls, err)
		}
		if err != nil && strings.Contains(err.Error(), token) {
			t.Fatal("Boards probe leaked PAT in provider error")
		}
	}
}

func TestADOBacklogProjectUsesIndependentBoardsProject(t *testing.T) {
	cfg := &instance.Config{Repos: []instance.RepoRef{{Provider: "ado", Owner: "org", Project: "code", Name: "web", Token: instance.TokenRef{Env: "ADO_PAT"}}}}
	set := &instance.ConfigSet{Gaggles: []apiv1.Gaggle{{Spec: apiv1.GaggleSpec{
		Project: apiv1.RepoRef{Provider: apiv1.ProviderADO, Owner: "org", Project: "code", Name: "web"},
		Backlog: apiv1.BacklogRef{Provider: apiv1.ProviderADO, Project: "work"},
	}}}}
	previous := targetADOBacklogReachable
	calls := 0
	targetADOBacklogReachable = func(ctx context.Context, repo instance.RepoRef, project string, stores credentials.StoreResolver) error {
		calls++
		if repo.Project != "code" || repo.Token.Env != "ADO_PAT" || project != "work" {
			t.Fatalf("incorrect credential/project binding: %+v project=%s", repo, project)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("unbounded preflight")
		}
		return nil
	}
	t.Cleanup(func() { targetADOBacklogReachable = previous })
	var out strings.Builder
	if !checkADOBacklogProjects(".", "config", cfg, set, nil, &out, &diagnosticCollector{}) || calls != 1 {
		t.Fatalf("independent Boards project refused: %s calls=%d", out.String(), calls)
	}
	set.Gaggles[0].Spec.Backlog.Project = ""
	if checkADOBacklogProjects(".", "config", cfg, set, nil, &out, &diagnosticCollector{}) || calls != 1 {
		t.Fatal("empty Boards project accepted or queried")
	}
}

func TestValidateADOBacklogProjectFailureJSON(t *testing.T) {
	t.Setenv("GOOBERS_ADO_TOKEN", "")
	root := filepath.Join(t.TempDir(), "ado")
	code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	code, _, stderr = runArgs(t, "connect", "org/code/web", root)
	if code != 0 {
		t.Fatalf("connect: %d %s", code, stderr)
	}
	t.Setenv("GOOBERS_ADO_TOKEN", "test-pat")
	stubConnectReachability(t, nil)
	previous := targetADOBacklogReachable
	targetADOBacklogReachable = func(context.Context, instance.RepoRef, string, credentials.StoreResolver) error {
		return errors.New("project not found or Work Items permission denied")
	}
	t.Cleanup(func() { targetADOBacklogReachable = previous })
	code, stdout, stderr := runArgs(t, "validate", "--strict", "--check-repos", "--json", root)
	if code != 1 {
		t.Fatalf("validate: %d stdout=%s stderr=%s", code, stdout, stderr)
	}
	for _, want := range []string{"BACKLOG001", "/spec/backlog/project", "Work Items", "gaggle.yaml"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("JSON lacks %q: %s", want, stdout)
		}
	}
}
