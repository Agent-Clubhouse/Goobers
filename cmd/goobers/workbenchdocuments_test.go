package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/workbench"
	"github.com/goobers/goobers/internal/workbenchgraph"
	"github.com/goobers/goobers/internal/workbenchprovider"
	"github.com/goobers/goobers/internal/workbenchservice"
	"github.com/goobers/goobers/providers"
)

type hostDocumentReader struct {
	t      *testing.T
	target apiv1.RepoRef
	reads  int
}

func (*hostDocumentReader) Kind() providers.ProviderKind { return providers.ProviderGitHub }
func (r *hostDocumentReader) ReadSourceBranch(_ context.Context, repo providers.RepositoryRef, branch string) (string, error) {
	if repo.Owner != r.target.Owner || repo.Name != r.target.Name || branch != "strategy" {
		r.t.Fatal("repository branch escaped configuration")
	}
	return strings.Repeat("a", 40), nil
}
func (r *hostDocumentReader) ReadRepositorySource(_ context.Context, repo providers.RepositoryRef, path, commit string) (providers.RepositorySourceFile, error) {
	if repo.Owner != r.target.Owner || repo.Name != r.target.Name || path != "plan.md" || commit != strings.Repeat("a", 40) {
		r.t.Fatal("source escaped declared path/commit")
	}
	r.reads++
	return providers.RepositorySourceFile{Commit: commit, Path: path, BlobID: "e69de29bb2d1d6434b8b29ae775ad8c2e48c5391", Content: []byte{}}, nil
}
func TestWorkbenchHostInstallsExactRepositoryDocumentAuthority(t *testing.T) {
	setup, pin := interactiveExecutionFixture(t)
	g := setup.Definitions.Gaggles[0].DeepCopy()
	g.Spec.Project.Branch = "strategy"
	target := interactiveRepository(g.Spec.Project)
	g.Spec.Workbench = &apiv1.GaggleWorkbench{SchemaVersion: "sources/v1", Sources: []apiv1.WorkbenchSource{{Name: "strategy", Kind: "documents", Repository: &target, Paths: []string{"plan.md"}}}}
	if err := setup.InteractiveAccess.Apply([]apiv1.Gaggle{*g}, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HUMAN_REPO", "repository-read-canary")
	t.Setenv("GH_TOKEN", "automation-must-not-be-used")
	reader := &hostDocumentReader{t: t, target: g.Spec.Project}
	service := &workbenchservice.Service{Permissions: setup.InteractiveAccess, Repository: func(_ context.Context, binding workbenchservice.ReadBinding, credential interactiveaccess.Credential) (workbenchprovider.RepositoryClient, error) {
		if credential.Value != "repository-read-canary" || binding.Scope.GaggleID != g.Name || binding.Source.Spec.Name != "strategy" || *binding.Source.Spec.Repository != target {
			t.Fatal("document factory lost exact human/source authority")
		}
		return reader, nil
	}}
	u := &upSession{}
	u.setup, u.l = setup, pin.layout
	u.installWorkbenchReads(service)
	p := httpapi.Principal{Issuer: "issuer", Subject: "human", Roles: []httpapi.Role{httpapi.RoleOperate}}
	capabilities, err := setup.InteractiveAccess.InteractiveCapabilities(t.Context(), p, g.Name)
	if err != nil {
		t.Fatal(err)
	}
	available := false
	for _, action := range capabilities.Actions {
		if action.Action == "repository.read" {
			available = action.Available
		}
		if action.Action == "backlog.read" && action.Available {
			t.Fatal("missing backlog source advertised")
		}
	}
	if !available {
		t.Fatal("installed document reader not advertised")
	}
	options := append(u.apiHandlerOpts, httpapi.WithAuthenticator(interactiveTestAuthenticator{principal: &p}))
	handler, err := httpapi.NewHandler(&readservice.Local{}, httpapi.RequireRoles(), log.New(io.Discard, "", 0), options...)
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/gaggles/example/workbench/sources/strategy/documents"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	var page workbench.DocumentPage
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &page) != nil || page.Coverage != "complete" || len(page.Files) != 1 || page.Files[0].Status != "available" || reader.reads != 1 {
		t.Fatalf("document read failed: %d %s", response.Code, response.Body)
	}
	graphPath := "/api/v1/gaggles/example/workbench/graph"
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, graphPath, nil))
	var graph workbenchgraph.Graph
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &graph) != nil || graph.Generation == "" || len(graph.Sources) != 1 || graph.Sources[0].Status != "complete" || reader.reads != 2 {
		t.Fatalf("installed graph lost document authority: %d %s", response.Code, response.Body)
	}
	changed := g.DeepCopy()
	changed.Spec.InteractiveAccess.Actions = []apiv1.InteractiveAction{"backlog.read"}
	if err = setup.InteractiveAccess.Apply([]apiv1.Gaggle{*changed}, nil); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
	if response.Code != 403 || reader.reads != 2 {
		t.Fatal("revoked repository read reached source", response.Code)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, graphPath, nil))
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &graph) != nil || len(graph.Sources) != 1 || graph.Sources[0].Status != "not-read" || len(graph.Documents) != 0 || reader.reads != 2 {
		t.Fatalf("graph leaked revoked source: %d %s", response.Code, response.Body)
	}
}
