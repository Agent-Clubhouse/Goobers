package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/workspacerevision"
)

func podRevisionFixture() (*apiv1.WorkspaceRevision, dispatcher.WorkspaceCheckout) {
	source := apiv1.RepoRef{Provider: apiv1.ProviderGitHub, Owner: "fork", Name: "repo"}
	return &apiv1.WorkspaceRevision{
		Repository: apiv1.RepositoryIdentity{Provider: source.Provider, Owner: source.Owner, Name: source.Name},
		CommitSHA:  strings.Repeat("a", 40), SourceRef: "refs/heads/same-name",
	}, dispatcher.WorkspaceCheckout{Repository: source}
}

func stampPodRevision(t *testing.T, revision *apiv1.WorkspaceRevision, checkout dispatcher.WorkspaceCheckout) {
	t.Helper()
	identity, err := json.Marshal(revision)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := json.Marshal(checkout)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(dispatcher.EnvWorkspaceRevision, string(identity))
	t.Setenv(dispatcher.EnvWorkspaceCheckout, string(policy))
	t.Setenv(dispatcher.EnvStageWorkspace, string(apiv1.WorkspaceRepoReadOnly))
	for _, name := range []string{dispatcher.EnvWorkspaceBranch, dispatcher.EnvWorkspaceDelta, dispatcher.EnvStageSyncBase, dispatcher.EnvCheckoutCapability} {
		t.Setenv(name, "")
	}
}

func TestPodWorkspaceRevisionParsing(t *testing.T) {
	for _, tc := range []struct{ name, key, value, code string }{
		{"valid", "", "", ""},
		{"missing identity", dispatcher.EnvWorkspaceRevision, "", workspacerevision.CodeInvalid},
		{"missing authorization", dispatcher.EnvWorkspaceCheckout, "", workspacerevision.CodeUnauthorized},
		{"malformed", dispatcher.EnvWorkspaceRevision, "{", workspacerevision.CodeInvalid},
		{"null identity", dispatcher.EnvWorkspaceRevision, "null", workspacerevision.CodeInvalid},
		{"unknown transport", dispatcher.EnvWorkspaceCheckout, `{"repository":{},"token":"bad"}`, workspacerevision.CodeInvalid},
		{"trailing", dispatcher.EnvWorkspaceRevision, "{} {}", workspacerevision.CodeInvalid},
		{"wrong repo", dispatcher.EnvWorkspaceCheckout, `{"repository":{"provider":"github","owner":"base","name":"repo"},"partialClone":false}`, workspacerevision.CodeUnauthorized},
		{"writable", dispatcher.EnvStageWorkspace, "repo", workspacerevision.CodeInvalid},
		{"branch", dispatcher.EnvWorkspaceBranch, "same-name", workspacerevision.CodeConflict},
		{"delta", dispatcher.EnvWorkspaceDelta, "digest", workspacerevision.CodeConflict},
		{"sync", dispatcher.EnvStageSyncBase, "true", workspacerevision.CodeConflict},
		{"base credential", dispatcher.EnvCheckoutCapability, "repo:push", workspacerevision.CodeConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			revision, checkout := podRevisionFixture()
			stampPodRevision(t, revision, checkout)
			if tc.key != "" {
				t.Setenv(tc.key, tc.value)
			}
			got, policy, err := podWorkspaceRevision()
			if tc.code == "" {
				if err != nil || !reflect.DeepEqual(got, revision) || !reflect.DeepEqual(policy, &checkout) {
					t.Fatalf("decode=%+v %+v %v", got, policy, err)
				}
			} else {
				var refusal *workspacerevision.Error
				if !errors.As(err, &refusal) || refusal.Code != tc.code || !refusal.NonRetryable() {
					t.Fatalf("error=%v, want %s", err, tc.code)
				}
			}
		})
	}
}

func TestPodWorkspaceRevisionNoDeltaOrCredentialFallback(t *testing.T) {
	revision, checkout := podRevisionFixture()
	stampPodRevision(t, revision, checkout)
	dir := t.TempDir()
	err := checkoutRepoWorkspace(context.Background(), dir, io.Discard,
		[]dispatcher.MintedCredential{{Capability: "repo:push", Value: "base-only"}})
	if got := podWorkspaceFailureCode("", err); got != workspacerevision.CodeUnauthorized {
		t.Fatalf("base token substituted: %v", err)
	}
	for _, creds := range [][]dispatcher.MintedCredential{
		nil,
		{{Capability: dispatcher.WorkspaceRevisionCheckoutCapability}},
		{{Capability: dispatcher.WorkspaceRevisionCheckoutCapability, Anonymous: true, Value: "contradictory"}},
		{{Capability: dispatcher.WorkspaceRevisionCheckoutCapability, Value: "one"}, {Capability: dispatcher.WorkspaceRevisionCheckoutCapability}},
		{{Capability: dispatcher.WorkspaceRevisionCheckoutCapability, Anonymous: true}, {Capability: dispatcher.WorkspaceRevisionCheckoutCapability, Anonymous: true}},
	} {
		err := checkoutRepoWorkspace(context.Background(), dir, io.Discard, creds)
		if podWorkspaceFailureCode("", err) != workspacerevision.CodeUnauthorized {
			t.Fatalf("invalid source grants accepted: %+v, %v", creds, err)
		}
	}
	// Even a mutated workspace mode cannot enter the publication/consume path.
	t.Setenv(dispatcher.EnvStageWorkspace, "repo")
	t.Setenv(dispatcher.EnvWorkspaceDelta, "must-not-fetch")
	if err := applyStageWorkspaceDelta(context.Background(), dir, nil, io.Discard); err != nil {
		t.Fatal(err)
	}
	delta, err := publishWorkspaceDelta(context.Background(), dir, io.Discard)
	if err != nil || delta != (publishedWorkspaceDelta{}) {
		t.Fatalf("selected revision published delta: %+v %v", delta, err)
	}
	t.Setenv(dispatcher.EnvStageIsCLI, "true")
	for _, entry := range stageEnvironment() {
		if strings.HasPrefix(entry, dispatcher.EnvWorkspaceRevision+"=") || strings.HasPrefix(entry, dispatcher.EnvWorkspaceCheckout+"=") {
			t.Fatal("selected checkout controls exposed to stage")
		}
	}
}

func TestPodWorkspaceRevisionSterileEnvironment(t *testing.T) {
	got := sterileRevisionEnvironment([]string{
		"PATH=path", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=filter.custom.smudge",
		"GIT_CONFIG_VALUE_0=malicious", "GIT_DIR=wrong", "GIT_WORK_TREE=wrong",
		"GIT_OBJECT_DIRECTORY=wrong", "GIT_ALTERNATE_OBJECT_DIRECTORIES=wrong",
		"GIT_ASKPASS=trusted-helper", "GOOBERS_GIT_TOKEN=source-token",
	})
	env := map[string]string{}
	for _, entry := range got {
		key, value, _ := strings.Cut(entry, "=")
		env[key] = value
	}

	for _, name := range []string{"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_DIR", "GIT_WORK_TREE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES"} {
		if _, ok := env[name]; ok {
			t.Fatalf("ambient %s retained", name)
		}
	}
	if env["GIT_CONFIG_GLOBAL"] != os.DevNull || env["GIT_CONFIG_SYSTEM"] != os.DevNull ||
		env["GIT_LFS_SKIP_SMUDGE"] != "1" || env["GIT_ASKPASS"] != "trusted-helper" {
		t.Fatalf("sterile policy missing: %v", env)
	}
}

func TestPodWorkspaceRevisionFailureRetryability(t *testing.T) {
	for _, code := range []string{workspacerevision.CodeUnauthorized, workspacerevision.CodeInvalid,
		workspacerevision.CodeConflict, workspacerevision.CodeObjectType,
		workspacerevision.CodeSHAMismatch, workspacerevision.CodeAcquisition} {
		err := &workspacerevision.Error{Code: code, Message: "refused"}
		result := podWorkspaceFailure("workspace_provision_failed", err)
		if result.Error.Code != code || result.Error.Retryable != !err.NonRetryable() {
			t.Fatalf("shared failure class changed: %+v", result.Error)
		}
	}
}

func TestPodWorkspaceRevisionResultControl(t *testing.T) {
	revision, _ := podRevisionFixture()
	data, err := json.Marshal(map[string]any{"workspaceRevision": revision, "other": "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := podResultWorkspaceRevision(data)
	if err != nil || !reflect.DeepEqual(got, revision) {
		t.Fatalf("selected control=%+v %v", got, err)
	}
	for _, raw := range []string{`null`, `"main"`, `{}`, `{"commitSha":"main"}`, `{"repository":{"provider":"github","owner":"fork","name":"repo"},"commitSha":"` + strings.Repeat("a", 40) + `","token":"forbidden"}`} {
		if _, err := podResultWorkspaceRevision([]byte(`{"workspaceRevision":` + raw + `}`)); podWorkspaceFailureCode("", err) != workspacerevision.CodeInvalid {
			t.Fatalf("invalid selected control accepted: %s, %v", raw, err)
		}
	}
	for _, data := range []string{`{"other":"legacy"}`, "not json"} {
		if got, err := podResultWorkspaceRevision([]byte(data)); got != nil || err != nil {
			t.Fatalf("legacy result changed: %+v %v", got, err)
		}
	}
}
