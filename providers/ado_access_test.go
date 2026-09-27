package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func newADOAccessTestProvider(t *testing.T, mux *http.ServeMux) *ADOProvider {
	t.Helper()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return NewADOProvider("example-org", "example-project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
}

func adoAccessRepo() RepositoryRef {
	return RepositoryRef{Provider: ProviderADO, Owner: "example-org", Project: "example-project", Name: "web"}
}

func TestADOProviderRepositoryIDs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/example-org/example-project/_apis/git/repositories/web", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{
			"id":            "repo-guid",
			"defaultBranch": "refs/heads/main",
			"project":       map[string]string{"id": "project-guid"},
		})
	})
	provider := newADOAccessTestProvider(t, mux)
	ids, err := provider.RepositoryIDs(context.Background(), adoAccessRepo())
	if err != nil {
		t.Fatalf("RepositoryIDs: %v", err)
	}
	want := ADORepositoryIDs{ProjectID: "project-guid", RepositoryID: "repo-guid", DefaultBranch: "refs/heads/main"}
	if ids != want {
		t.Fatalf("ids = %+v, want %+v", ids, want)
	}
}

func TestADOProviderRepositoryIDsRequiresIDs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/example-org/example-project/_apis/git/repositories/web", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, map[string]interface{}{"id": "repo-guid"})
	})
	provider := newADOAccessTestProvider(t, mux)
	if _, err := provider.RepositoryIDs(context.Background(), adoAccessRepo()); err == nil {
		t.Fatal("RepositoryIDs returned nil error for a response with no project id")
	}
}

func TestADOProviderEvaluateGitPermissions(t *testing.T) {
	var got adoPermissionEvaluationBatch
	mux := http.NewServeMux()
	mux.HandleFunc("/example-org/_apis/security/permissionevaluationbatch", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodPost)
		if version := r.URL.Query().Get("api-version"); version != "7.1" {
			t.Errorf("api-version = %q, want 7.1", version)
		}
		decodeJSON(t, r, &got)
		response := got
		response.Evaluations = nil
		for _, evaluation := range got.Evaluations {
			// Contribute is allowed; every other permission is denied.
			value := evaluation.Permissions == int(ADOGitContribute)
			evaluation.Value = &value
			response.Evaluations = append(response.Evaluations, evaluation)
		}
		writeJSON(t, w, response)
	})
	provider := newADOAccessTestProvider(t, mux)
	permissions := []ADOGitPermission{ADOGitContribute, ADOGitPullRequestPolicyOverride}
	results, err := provider.EvaluateGitPermissions(context.Background(),
		ADORepositoryIDs{ProjectID: "project-guid", RepositoryID: "repo-guid"}, permissions)
	if err != nil {
		t.Fatalf("EvaluateGitPermissions: %v", err)
	}
	want := map[ADOGitPermission]bool{ADOGitContribute: true, ADOGitPullRequestPolicyOverride: false}
	if !reflect.DeepEqual(results, want) {
		t.Fatalf("results = %v, want %v", results, want)
	}
	if got.AlwaysAllowAdministrators {
		t.Error("alwaysAllowAdministrators = true; the check must report effective access, not assume it for administrators")
	}
	if len(got.Evaluations) != 2 {
		t.Fatalf("evaluations = %+v, want 2", got.Evaluations)
	}
	for _, evaluation := range got.Evaluations {
		if evaluation.SecurityNamespaceID != adoGitRepositoriesNamespace {
			t.Errorf("namespace = %q, want the Git repositories namespace", evaluation.SecurityNamespaceID)
		}
		if evaluation.Token != "repoV2/project-guid/repo-guid" {
			t.Errorf("token = %q, want repoV2/project-guid/repo-guid", evaluation.Token)
		}
	}
}

func TestADOProviderEvaluateGitPermissionsMissingValueIsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/example-org/_apis/security/permissionevaluationbatch", func(w http.ResponseWriter, r *http.Request) {
		var request adoPermissionEvaluationBatch
		decodeJSON(t, r, &request)
		writeJSON(t, w, request) // no "value" on any evaluation
	})
	provider := newADOAccessTestProvider(t, mux)
	_, err := provider.EvaluateGitPermissions(context.Background(),
		ADORepositoryIDs{ProjectID: "project-guid", RepositoryID: "repo-guid"}, []ADOGitPermission{ADOGitCreateBranch})
	if err == nil || !strings.Contains(err.Error(), "Create branch") {
		t.Fatalf("err = %v, want an error naming Create branch", err)
	}
}

func TestADOProviderBlanketPrefixPolicies(t *testing.T) {
	scope := func(repositoryID, refName, matchKind string) map[string]string {
		return map[string]string{"repositoryId": repositoryID, "refName": refName, "matchKind": matchKind}
	}
	config := func(id int, name string, enabled, blocking bool, scopes ...map[string]string) map[string]interface{} {
		return map[string]interface{}{
			"id": id, "isEnabled": enabled, "isBlocking": blocking,
			"type":     map[string]string{"displayName": name},
			"settings": map[string]interface{}{"scope": scopes},
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/example-org/example-project/_apis/policy/configurations", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"value": []interface{}{
			config(7, "Build", true, true, scope("repo-guid", "refs/heads/", "Prefix")),
			config(3, "Minimum number of reviewers", true, true, scope("", "refs/heads", "prefix")),
			config(4, "Build", true, true, scope("repo-guid", "refs/heads/main", "Exact")),
			config(5, "Build", true, true, scope("repo-guid", "refs/heads/release/", "Prefix")),
			config(6, "Build", true, false, scope("repo-guid", "refs/heads/", "Prefix")),
			config(8, "Build", false, true, scope("repo-guid", "refs/heads/", "Prefix")),
			config(9, "Build", true, true, scope("other-repo-guid", "refs/heads/", "Prefix")),
			config(10, "File size restriction", true, true, scope("repo-guid", "", "")),
			config(11, "Build", true, true),
		}})
	})
	provider := newADOAccessTestProvider(t, mux)
	policies, err := provider.BlanketPrefixPolicies(context.Background(), adoAccessRepo(), "repo-guid")
	if err != nil {
		t.Fatalf("BlanketPrefixPolicies: %v", err)
	}
	want := []ADOBranchPolicy{
		{ID: 3, TypeName: "Minimum number of reviewers", RefName: "refs/heads"},
		{ID: 7, TypeName: "Build", RefName: "refs/heads/"},
	}
	if !reflect.DeepEqual(policies, want) {
		t.Fatalf("policies = %+v, want %+v", policies, want)
	}
}

func TestADOProviderWorkItemTypeStatesAndDefaultCreateType(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/example-org/example-project/_apis/wit/workitemtypecategories/Microsoft.RequirementCategory", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"defaultWorkItemType": map[string]string{"name": "Story"}})
	})
	mux.HandleFunc("/example-org/example-project/_apis/wit/workitemtypes/Story/states", func(w http.ResponseWriter, r *http.Request) {
		assertMethod(t, r, http.MethodGet)
		writeJSON(t, w, map[string]interface{}{"value": []map[string]string{
			{"name": "New", "category": "Proposed"},
			{"name": "Closed", "category": "Completed"},
		}})
	})
	provider := newADOAccessTestProvider(t, mux)
	itemType, err := provider.DefaultCreateType(context.Background(), adoAccessRepo())
	if err != nil || itemType != "Story" {
		t.Fatalf("DefaultCreateType = %q, %v; want Story", itemType, err)
	}
	states, err := provider.WorkItemTypeStates(context.Background(), adoAccessRepo(), itemType)
	if err != nil {
		t.Fatalf("WorkItemTypeStates: %v", err)
	}
	want := []ADOWorkItemState{{Name: "New", Category: "Proposed"}, {Name: "Closed", Category: "Completed"}}
	if !reflect.DeepEqual(states, want) {
		t.Fatalf("states = %+v, want %+v", states, want)
	}
}
