package schemas

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

func TestWorkspaceRevisionResultSchema(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	for _, file := range Files() {
		raw, err := FS.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource(BaseURI+file, bytes.NewReader(raw)); err != nil {
			t.Fatal(err)
		}
	}
	schema, err := compiler.Compile(BaseURI + "result.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	sha := strings.Repeat("a", 40)
	repository := `{"provider":"github","owner":"org","name":"repo"}`
	revision := `{"repository":` + repository + `,"commitSha":"` + sha + `"}`
	for _, tc := range []struct {
		name  string
		raw   string
		valid bool
	}{
		{"legacy success", `{"status":"success"}`, true},
		{"legacy failure", `{"status":"failure","error":{"code":"FAILED","message":"failed"}}`, true},
		{"selected", `{"status":"success","workspaceRevision":` + revision + `}`, true},
		{"SHA256", `{"status":"success","workspaceRevision":` + strings.Replace(revision, sha, strings.Repeat("b", 64), 1) + `}`, true},
		{"independent workspaceBranch", `{"status":"success","outputs":{"workspaceBranch":"goobers/run"},"workspaceRevision":` + revision + `}`, true},
		{"null revision", `{"status":"success","workspaceRevision":null}`, false},
		{"missing repository", `{"status":"success","workspaceRevision":{"commitSha":"` + sha + `"}}`, false},
		{"missing SHA", `{"status":"success","workspaceRevision":{"repository":` + repository + `}}`, false},
		{"uppercase SHA", `{"status":"success","workspaceRevision":` + strings.Replace(revision, sha, strings.ToUpper(sha), 1) + `}`, false},
		{"short SHA", `{"status":"success","workspaceRevision":` + strings.Replace(revision, sha, sha[:39], 1) + `}`, false},
		{"ref expression", `{"status":"success","workspaceRevision":` + strings.Replace(revision, sha, sha+"^0", 1) + `}`, false},
		{"unknown revision field", `{"status":"success","workspaceRevision":` + strings.TrimSuffix(revision, "}") + `,"branch":"main"}}`, false},
		{"unknown result field", `{"status":"success","workspaceRevision":` + revision + `,"routing":"main"}`, false},
		{"ADO missing project", `{"status":"success","workspaceRevision":` + strings.Replace(revision, `"github"`, `"ado"`, 1) + `}`, false},
		{"Gitea missing host", `{"status":"success","workspaceRevision":` + strings.Replace(revision, `"github"`, `"gitea"`, 1) + `}`, false},
		{"unknown provider", `{"status":"success","workspaceRevision":` + strings.Replace(revision, `"github"`, `"other"`, 1) + `}`, false},
		{"identity path injection", `{"status":"success","workspaceRevision":` + strings.Replace(revision, `"owner":"org"`, `"owner":"../org"`, 1) + `}`, false},
		{"invalid base SHA", `{"status":"success","workspaceRevision":` + strings.TrimSuffix(revision, "}") + `,"baseSha":"main"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var value any
			if err := json.Unmarshal([]byte(tc.raw), &value); err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(value); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	for _, field := range []string{"branch", "checkout", "connectionRef", "baseUrl", "credentials", "unknown"} {
		for _, slot := range []string{"repository", "baseRepository"} {
			t.Run("closed "+slot+" "+field, func(t *testing.T) {
				var value map[string]any
				raw := `{"status":"success","workspaceRevision":` + revision + `}`
				if err := json.Unmarshal([]byte(raw), &value); err != nil {
					t.Fatal(err)
				}
				selected := value["workspaceRevision"].(map[string]any)
				selected[slot] = map[string]any{"provider": "github", "owner": "org", "name": "repo", field: "injected"}
				if err := schema.Validate(value); err == nil {
					t.Fatal("schema accepted stage-supplied authority")
				}
			})
		}
	}
	base := apiv1.RepositoryIdentity{Provider: "ado", Owner: "org", Project: "project", Name: "repo", ID: "native", URL: "https://dev.azure.com/org/project/_git/repo"}
	result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, WorkspaceRevision: &apiv1.WorkspaceRevision{
		Repository: base, CommitSHA: sha, SourceRef: "refs/heads/topic", SourceID: "123", BaseRepository: &base, BaseSHA: strings.Repeat("b", 64),
	}}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(value); err != nil {
		t.Fatalf("complete typed roundtrip: %v", err)
	}
}
