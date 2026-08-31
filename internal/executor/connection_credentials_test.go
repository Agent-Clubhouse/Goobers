package executor

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
)

// stageCredentialKeys is the seam that lets one stage hold two independently
// scoped credentials at once (personal-gaggle-routing §5.2): its declared
// capabilities for the project repository, plus the connection credential its
// gaggle's backlog references.

func TestStageCredentialKeysRequestsBacklogConnection(t *testing.T) {
	env := apiv1.InvocationEnvelope{
		Capabilities: []string{"github:issues:write"},
		BacklogRef: &apiv1.BacklogRef{
			Provider:      apiv1.ProviderGitHub,
			Project:       "gim-home/brandiv.goobers",
			ConnectionRef: "private-backlog",
		},
	}
	got := stageCredentialKeys(env)
	want := credentials.ConnectionCredentialKey("private-backlog")
	if !containsString(got, "github:issues:write") {
		t.Fatalf("keys = %v, want the declared capability preserved", got)
	}
	if !containsString(got, want) {
		t.Fatalf("keys = %v, want the backlog connection key %q", got, want)
	}
}

// TestStageCredentialKeysUnchangedWithoutConnection keeps the same-project /
// same-backlog majority on byte-identical behavior: no connectionRef means no
// extra credential key is even requested.
func TestStageCredentialKeysUnchangedWithoutConnection(t *testing.T) {
	capabilities := []string{"github:issues:write", "repo:push"}
	for name, env := range map[string]apiv1.InvocationEnvelope{
		"no backlog ref": {Capabilities: capabilities},
		"no connection": {
			Capabilities: capabilities,
			BacklogRef:   &apiv1.BacklogRef{Provider: apiv1.ProviderGitHub, Project: "o/r"},
		},
	} {
		got := stageCredentialKeys(env)
		if len(got) != len(capabilities) {
			t.Errorf("%s: keys = %v, want exactly the declared capabilities", name, got)
		}
	}
}

// TestStageCredentialKeysDoesNotDuplicate guards the case where a stage somehow
// already declares the connection key: the injector rejects duplicate grants,
// so requesting it twice must be impossible.
func TestStageCredentialKeysDoesNotDuplicate(t *testing.T) {
	key := credentials.ConnectionCredentialKey("private-backlog")
	env := apiv1.InvocationEnvelope{
		Capabilities: []string{key},
		BacklogRef: &apiv1.BacklogRef{
			Provider:      apiv1.ProviderGitHub,
			Project:       "o/r",
			ConnectionRef: "private-backlog",
		},
	}
	got := stageCredentialKeys(env)
	count := 0
	for _, k := range got {
		if k == key {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("key %q appears %d times in %v, want exactly once", key, count, got)
	}
}

// TestStageCredentialKeysDoesNotMutateEnvelope keeps the envelope's declared
// capability slice from being aliased and appended to in place.
func TestStageCredentialKeysDoesNotMutateEnvelope(t *testing.T) {
	capabilities := make([]string, 1, 4) // spare capacity is what makes aliasing possible
	capabilities[0] = "github:issues:write"
	env := apiv1.InvocationEnvelope{
		Capabilities: capabilities,
		BacklogRef: &apiv1.BacklogRef{
			Provider:      apiv1.ProviderGitHub,
			Project:       "o/r",
			ConnectionRef: "private-backlog",
		},
	}
	_ = stageCredentialKeys(env)
	if len(env.Capabilities) != 1 || env.Capabilities[0] != "github:issues:write" {
		t.Fatalf("envelope capabilities were mutated: %v", env.Capabilities)
	}
}

func TestConnectionCredentialKeyRoundTrip(t *testing.T) {
	key := credentials.ConnectionCredentialKey("private-backlog")
	if !credentials.IsConnectionCredentialKey(key) {
		t.Fatalf("%q should be recognized as a connection key", key)
	}
	name, ok := credentials.ConnectionCredentialName(key)
	if !ok || name != "private-backlog" {
		t.Fatalf("name = %q ok = %v, want private-backlog", name, ok)
	}
	if credentials.IsConnectionCredentialKey("github:issues:write") {
		t.Fatal("a capability must not be mistaken for a connection key")
	}
	// The injected env var must be distinct from any capability's, or one
	// credential would clobber the other in the stage environment.
	if CredentialEnvVar(key) == CredentialEnvVar("github:issues:write") {
		t.Fatal("connection and capability credentials must occupy distinct env vars")
	}
}
