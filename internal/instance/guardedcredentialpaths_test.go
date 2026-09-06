package instance

import (
	"reflect"
	"sort"
	"testing"
)

// TestGuardedCredentialPaths is #4273's coverage of the enumeration itself:
// every file-backed TokenRef this config type carries must be collected,
// and nothing else (a store-backed or env-backed ref carries no path to
// guard).
func TestGuardedCredentialPaths(t *testing.T) {
	cfg := &Config{
		Repos: []RepoRef{
			{
				Provider: "github",
				Owner:    "acme",
				Name:     "pat-repo",
				Token:    TokenRef{File: "/creds/pat-repo-token"},
			},
			{
				Provider: "github",
				Owner:    "acme",
				Name:     "app-repo",
				Auth: &RepoAuthConfig{
					Kind:       GitHubAuthApp,
					PrivateKey: &TokenRef{File: "/creds/app-repo-key.pem"},
				},
			},
			{
				// Env-backed token: no path to guard.
				Provider: "github",
				Owner:    "acme",
				Name:     "env-repo",
				Token:    TokenRef{Env: "ACME_ENV_REPO_TOKEN"},
			},
		},
		WorkflowSource: &WorkflowSource{
			Kind:  "git",
			Token: &TokenRef{File: "/creds/workflow-source-token"},
		},
		DaemonIdentity: &DaemonIdentityConfig{
			Kind:       GitHubAuthApp,
			PrivateKey: &TokenRef{File: "/creds/daemon-identity-key.pem"},
		},
		Credentials: []CredentialGrant{
			{Capability: "agent:model", Token: TokenRef{File: "/creds/agent-model-token"}},
			{Capability: "repo:push", Token: TokenRef{Env: "PUSH_TOKEN"}},
		},
	}

	got := GuardedCredentialPaths(cfg)
	sort.Strings(got)
	want := []string{
		"/creds/agent-model-token",
		"/creds/app-repo-key.pem",
		"/creds/daemon-identity-key.pem",
		"/creds/pat-repo-token",
		"/creds/workflow-source-token",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GuardedCredentialPaths = %v, want %v", got, want)
	}
}

// TestGuardedCredentialPathsNilConfigIsEmpty guards the zero-value/nil
// caller shape a wiring bug could produce.
func TestGuardedCredentialPathsNilConfigIsEmpty(t *testing.T) {
	if got := GuardedCredentialPaths(nil); len(got) != 0 {
		t.Fatalf("GuardedCredentialPaths(nil) = %v, want empty", got)
	}
	if got := GuardedCredentialPaths(&Config{}); len(got) != 0 {
		t.Fatalf("GuardedCredentialPaths(zero-value) = %v, want empty", got)
	}
}

// TestGuardedCredentialPathsReachesEveryTokenRefContainer is the drift guard
// the first version of this enumeration needed and did not have. It listed the
// TokenRef fields it knew about by hand, and had already missed two live ones —
// the GitHub webhook secret and an OTLP collector's auth headers — on the day
// it was written. Both are file-backed credentials on the same disk as the App
// key, so both were unguarded.
//
// The fix is structural (a reflective walk), so this test is too: it places a
// file-backed ref in every CONTAINER SHAPE the Config graph uses — a value
// struct field, a pointer field, a slice element, a nested struct two levels
// down, and a map value — and requires all of them back. A new field that
// carries a TokenRef in any of those shapes is then covered on arrival,
// without a matching edit here.
func TestGuardedCredentialPathsReachesEveryTokenRefContainer(t *testing.T) {
	cfg := &Config{
		// Value struct field, nested one level (Config.Webhook.Secret).
		Webhook: WebhookConfig{Secret: TokenRef{File: "/creds/webhook-secret"}},
		// Slice element, nested two levels (Config.Repos[i].Auth.PrivateKey).
		Repos: []RepoRef{{
			Provider: "github",
			Owner:    "acme",
			Name:     "widgets",
			Auth: &RepoAuthConfig{
				Kind:       GitHubAuthApp,
				PrivateKey: &TokenRef{File: "/creds/app-key.pem"},
			},
		}},
		// Pointer field (Config.WorkflowSource.Token).
		WorkflowSource: &WorkflowSource{Kind: "git", Token: &TokenRef{File: "/creds/workflow-source-token"}},
		// Map value, nested two levels (Config.Telemetry.OTLP.Headers[k]).
		Telemetry: TelemetryConfig{OTLP: &OTLPConfig{
			Endpoint: "collector.example:4317",
			Headers:  map[string]TokenRef{"authorization": {File: "/creds/otlp-header"}},
		}},
	}

	got := GuardedCredentialPaths(cfg)
	want := []string{
		"/creds/app-key.pem",
		"/creds/otlp-header",
		"/creds/webhook-secret",
		"/creds/workflow-source-token",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GuardedCredentialPaths = %v, want %v (sorted, deduplicated)", got, want)
	}
}

// TestGuardedCredentialPathsDeduplicates covers the shape a real instance
// actually has: several refs pointing at the same mounted file. The executor
// compares each guarded path against every stage word, so a duplicate is pure
// repeated work — and a caller diffing two loads would see spurious churn.
func TestGuardedCredentialPathsDeduplicates(t *testing.T) {
	shared := "/creds/shared-app-key.pem"
	cfg := &Config{
		Repos: []RepoRef{
			{Provider: "github", Owner: "acme", Name: "a", Auth: &RepoAuthConfig{Kind: GitHubAuthApp, PrivateKey: &TokenRef{File: shared}}},
			{Provider: "github", Owner: "acme", Name: "b", Auth: &RepoAuthConfig{Kind: GitHubAuthApp, PrivateKey: &TokenRef{File: shared}}},
		},
		DaemonIdentity: &DaemonIdentityConfig{Kind: GitHubAuthApp, PrivateKey: &TokenRef{File: shared}},
	}
	if got := GuardedCredentialPaths(cfg); !reflect.DeepEqual(got, []string{shared}) {
		t.Fatalf("GuardedCredentialPaths = %v, want exactly [%s]", got, shared)
	}
}

// TestGuardedCredentialPathsIgnoresNonFileRefs pins the negative half: a ref
// with no file carries no path to guard, and inventing one would make the
// executor refuse stages over an env var name or a keychain service.
func TestGuardedCredentialPathsIgnoresNonFileRefs(t *testing.T) {
	cfg := &Config{
		Repos: []RepoRef{{Provider: "github", Owner: "acme", Name: "env", Token: TokenRef{Env: "ACME_TOKEN"}}},
		Credentials: []CredentialGrant{
			{Capability: "agent:model", Token: TokenRef{Keychain: "goobers-model"}},
			{Capability: "repo:push", Token: TokenRef{Store: "vault/push-token"}},
		},
	}
	if got := GuardedCredentialPaths(cfg); len(got) != 0 {
		t.Fatalf("GuardedCredentialPaths = %v, want empty for env/keychain/store-backed refs", got)
	}
}
