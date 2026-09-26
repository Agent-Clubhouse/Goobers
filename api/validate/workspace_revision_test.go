package validate

import (
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestWorkspaceRevisionIdentitySchemaMatchesRuntime(t *testing.T) {
	v, err := New()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]apiv1.RepositoryIdentity{
		"github":          {Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo"},
		"ado":             {Provider: apiv1.ProviderADO, Owner: "org", Project: "My Project", Name: "repo", ID: "123"},
		"gitea":           {Provider: apiv1.ProviderGitea, URL: "https://git.example.test:8443/forge", Owner: "org", Name: "repo"},
		"ipv6":            {Provider: apiv1.ProviderGitea, URL: "http://[::1]:3000", Owner: "org", Name: "repo"},
		"missing-project": {Provider: apiv1.ProviderADO, Owner: "org", Name: "repo"},
		"missing-url":     {Provider: apiv1.ProviderGitea, Owner: "org", Name: "repo"},
		"wrong-project":   {Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo", Project: "unexpected"},
	}
	for _, component := range []string{"", ".", "..", "../org", `org\repo`, "org|repo", " org", "org ", "org\n", "\u00a0org", "org\u0085", "org\x7f", "org\x85"} {
		cases["component-"+component] = apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: component, Name: "repo"}
	}
	for _, service := range []string{
		"ftp://example.test", "https://user@example.test", "https://example.test?q=x",
		"https://example.test?", "https://example.test#fragment", "https://example.test#",
		"https://example.test/a b", "https://example.test/a%20b", "https://example.test/%zz",
		"https://example.test\u00a0", "https://example.test:invalid", "https://",
		"https://example.test/\x00", "https://example.test/\x7f", "https://example.test/\u0080",
		"https://[]", "https://[broken", "https://example<test", "https://example\"test",
	} {
		cases["url-"+service] = apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo", URL: service}
	}
	for name, identity := range cases {
		t.Run(name, func(t *testing.T) {
			for _, base := range []bool{false, true} {
				revision := &apiv1.WorkspaceRevision{
					Repository: apiv1.RepositoryIdentity{Provider: apiv1.ProviderGitHub, Owner: "org", Name: "repo"},
					CommitSHA:  strings.Repeat("a", 40),
				}
				if base {
					revision.BaseRepository = &identity
				} else {
					revision.Repository = identity
				}
				valid := revision.Validate() == nil
				invocation, result, event := completeInvocationEnvelope(), completeResultEnvelope(), completeJournalEvent()
				invocation.WorkspaceRevision, result.WorkspaceRevision, event.WorkspaceRevision = revision, revision, revision
				for schema, value := range map[string]any{
					"workspace-revision.schema.json": revision,
					"invocation.schema.json":         invocation, "result.schema.json": result, "journal-event.schema.json": event,
				} {
					data, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					err = v.ValidateJSON(schema, data)
					if (err == nil) != valid {
						t.Errorf("%s base=%t: schema error=%v; runtime valid=%t", schema, base, err, valid)
					}
				}
			}
		})
	}
}
