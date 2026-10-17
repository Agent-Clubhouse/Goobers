package interactiveops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func dsl3Workflow(name, gaggle string) string {
	content := strings.Replace(dynamicWorkflow, `dslVersion: "2.0"`, `dslVersion: "3.0"`, 1)
	content = strings.Replace(content, "  annotations:", "  annotations:\n    goobers.dev/allow-preview-features: \"true\"", 1)
	content = strings.Replace(content, "name: arithmetic-review", "name: "+name, 1)
	content = strings.Replace(content, "gaggle: interactive", "gaggle: "+gaggle, 1)
	content = strings.ReplaceAll(content, "      run:", "      runsOn: {restrictions: [network:none]}\n      run:")
	return strings.ReplaceAll(content, "        network: none\n", "")
}
func nativePolicySource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"manifest.yaml":       "apiVersion: goobers.dev/v1alpha1\nkind: Manifest\nmetadata: {name: operator-source}\nspec:\n  instance: {name: source-instance, environment: dev}\n  gaggles: [team, other]\n",
		"team.yaml":           "apiVersion: goobers.dev/v1alpha1\nkind: Gaggle\ndslVersion: \"3.0\"\nmetadata: {name: team, annotations: {goobers.dev/allow-preview-features: \"true\"}}\nspec:\n  displayName: Actual configured workforce\n  project: {provider: github, owner: configured-owner, name: configured-repo, branch: main}\n  backlog: {provider: github, project: configured-owner/configured-repo}\n  isolation: {namespace: configured-team}\n",
		"other.yaml":          "apiVersion: goobers.dev/v1alpha1\nkind: Gaggle\nmetadata: {name: other}\nspec:\n  project: {provider: github, owner: other-owner, name: other-repo}\n  backlog: {provider: github, project: other-owner/other-repo}\n  isolation: {namespace: other}\n",
		"selected.yaml":       dsl3Workflow("selected-check", "team"),
		"private.yaml":        dsl3Workflow("private-check", "team"),
		"other-workflow.yaml": dsl3Workflow("other-check", "other"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateConfig(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}
func scopedNativeService(t *testing.T, binary string) (*Service, Scope, *Policy) {
	t.Helper()
	source := nativePolicySource(t)
	p := &Policy{Revision: "revision-one", Gaggle: "team", Workflows: []string{"selected-check"}, CommandCapabilities: []CommandCapability{CommandNodeCheck, CommandNodeTest}, AllowAuthoring: true}
	s, err := New(Options{Root: t.TempDir(), Repository: customRepository(t), Binary: binary, ConfigDirForScope: func(Scope) (string, error) { return source, nil }, PolicyForScope: func(Scope) (Policy, error) { return *p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return s, Scope{ActorID: "owner", SessionID: "native-policy"}, p
}
func TestNativePolicyProjectionAndScopedAuthoring(t *testing.T) {
	s, scope, _ := scopedNativeService(t, "")
	ctx := context.Background()
	state, err := s.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if state.Mode != "isolated-native-config" || len(state.Gaggles) != 1 || state.Gaggles[0].Name != "team" || state.Gaggles[0].Spec.Project.Owner != "configured-owner" || len(state.Workflows) != 1 || state.Workflows[0].Name != "selected-check" {
		t.Fatalf("not actual selected config: %+v", state)
	}
	if !strings.Contains(state.WorkflowDraft, `dslVersion: "3.0"`) || !strings.Contains(state.WorkflowDraft, "gaggle: team") || strings.Contains(state.WorkflowDraft, "network: none") {
		t.Fatalf("draft is not scoped DSL3: %s", state.WorkflowDraft)
	}
	if _, err = s.Propose(ctx, scope, Request{Kind: "tweak_workflow", Workflow: "private-check", Enabled: new(bool), IdempotencyKey: "not-allowed"}); err == nil {
		t.Fatal("non-allowlisted workflow accepted")
	}
	v, err := s.ValidateWorkflow(ctx, scope, dsl3Workflow("authored", "team"))
	if err != nil || !v.Valid {
		t.Fatalf("DSL3 validation: %+v %v", v, err)
	}
	a, err := s.Propose(ctx, scope, Request{Kind: "add_workflow", WorkflowYAML: v.NormalizedYAML, IdempotencyKey: "authored"})
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.Apply(ctx, scope, a.ID)
	if err != nil || a.Status != "succeeded" {
		t.Fatalf("authored apply: %+v %v", a, err)
	}
	enabled := false
	if _, err = s.Propose(ctx, scope, Request{Kind: "tweak_workflow", Workflow: "authored", Enabled: &enabled, IdempotencyKey: "own-new"}); err != nil {
		t.Fatalf("scope-owned authored workflow inaccessible: %v", err)
	}
	set, err := loadNativeConfig(filepath.Join(state.InstanceRoot, "config"))
	if err != nil || len(set.Workflows) != 2 {
		t.Fatalf("native config invalid: %+v %v", set, err)
	}
}
func TestPolicyRevisionAndRevocationKeepReceiptsAndCleanup(t *testing.T) {
	s, scope, p := scopedNativeService(t, "")
	ctx := context.Background()
	label := "reviewed name"
	a, err := s.Propose(ctx, scope, Request{Kind: "tweak_gaggle", DisplayName: &label, IdempotencyKey: "old-policy"})
	if err != nil {
		t.Fatal(err)
	}
	p.Revision = "revision-two"
	if _, err = s.Apply(ctx, scope, a.ID); err == nil {
		t.Fatal("policy revision changed without re-review")
	}
	if _, err = s.Get(ctx, scope, a.ID); err != nil {
		t.Fatalf("revision change hid receipt: %v", err)
	}
	s.opts.PolicyForScope = func(Scope) (Policy, error) { return Policy{}, errors.New("authorization revoked") }
	if _, err = s.Propose(ctx, scope, Request{Kind: "tweak_gaggle", DisplayName: &label, IdempotencyKey: "revoked"}); err == nil {
		t.Fatal("revoked mutation accepted")
	}
	if _, err = s.Get(ctx, scope, a.ID); err != nil {
		t.Fatalf("revocation hid receipt: %v", err)
	}
	if err = s.StopSession(ctx, scope); err != nil {
		t.Fatalf("revocation prevented cleanup: %v", err)
	}
	if err = s.StopSession(ctx, Scope{ActorID: scope.ActorID, SessionID: "never-used-helper"}); err != nil {
		t.Fatalf("stopping unused helper failed: %v", err)
	}
}
func TestExplicitEmptyPolicyAndUnknownCapabilitiesDeny(t *testing.T) {
	s, scope, p := scopedNativeService(t, "")
	ctx := context.Background()
	p.Workflows = nil
	p.CommandCapabilities = nil
	p.AllowAuthoring = false
	state, err := s.Snapshot(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Workflows) != 0 || len(state.CommandCapabilities) != 0 || state.WorkflowDraft != "" {
		t.Fatal("explicit empty policy fell back to defaults")
	}
	if _, err = s.Propose(ctx, scope, Request{Kind: "add_workflow", WorkflowYAML: dsl3Workflow("authored", "team"), IdempotencyKey: "forbidden"}); err == nil {
		t.Fatal("authoring forbidden policy bypass")
	}
	if v, err := s.ValidateWorkflow(ctx, scope, dsl3Workflow("authored", "team")); err != nil || v.Valid {
		t.Fatalf("empty command policy allowed execution: %+v %v", v, err)
	}
	s2, scope2, p2 := scopedNativeService(t, "")
	p2.CommandCapabilities = []CommandCapability{"shell"}
	if _, err = s2.Snapshot(ctx, scope2); err == nil {
		t.Fatal("unknown capability accepted")
	}
}
func TestDSL3RestrictionSurfaceFailsClosed(t *testing.T) {
	s, scope, _ := scopedNativeService(t, "")
	ctx := context.Background()
	for name, content := range map[string]string{
		"old-network":  strings.Replace(dsl3Workflow("candidate", "team"), "workspace: scratch", "workspace: scratch\n        network: none", 1),
		"allowlist":    strings.Replace(dsl3Workflow("candidate", "team"), "[network:none]", "[network:allowlist]", 1),
		"placement":    strings.Replace(dsl3Workflow("candidate", "team"), "runsOn: {restrictions:", "runsOn: {os: linux, restrictions:", 1),
		"cross-gaggle": dsl3Workflow("candidate", "other"),
	} {
		t.Run(name, func(t *testing.T) {
			v, err := s.ValidateWorkflow(ctx, scope, content)
			if err != nil || v.Valid {
				t.Fatalf("accepted unsafe DSL3: %+v %v", v, err)
			}
		})
	}
}

func TestNativeConfigPersonaAssetsAndSecretIsolation(t *testing.T) {
	source := nativePolicySource(t)
	for name, content := range map[string]string{
		"gaggles/team/goobers/helper/goober.yaml":          "apiVersion: goobers.dev/v1alpha1\nkind: Goober\nmetadata: {name: helper}\nspec:\n  gaggle: team\n  role: helper\n  instructions: instructions.md\n  workflows: [selected-check, private-check]\n  skills: [review]\n",
		"gaggles/team/goobers/helper/instructions.md":      "Read the actual selected configuration.\n",
		"gaggles/team/goobers/helper/assets/checklist.txt": "Review tests and syntax.\n",
		"gaggles/team/skills/review/SKILL.md":              "Review the configured tests.\n",
		".git/irrelevant":                                  "must not be copied\n",
		"secrets/token.txt":                                "controlled-secret-marker\n",
	} {
		path := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	gaggleFile := filepath.Join(source, "team.yaml")
	gaggleData, err := os.ReadFile(gaggleFile)
	if err != nil {
		t.Fatal(err)
	}
	gaggleData = append(gaggleData, []byte("  interactive:\n    surfaces:\n      - name: helper\n        mode: helper\n        gooberRef: helper\n        executionModes: [local-tools]\n        defaultExecutionMode: local-tools\n        tools: [session_context]\n        workflows: [selected-check, private-check]\n        session: {leaseSeconds: 30, maxTurnSeconds: 600, maxChildren: 1, maxActive: 2, maxActivePerOwner: 1}\n")...)
	if err = os.WriteFile(gaggleFile, gaggleData, 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateConfig(source); err != nil {
		t.Fatal(err)
	}
	p := Policy{Revision: "asset-config", Gaggle: "team", Workflows: []string{"selected-check"}, CommandCapabilities: []CommandCapability{CommandNodeCheck, CommandNodeTest}}
	s, err := New(Options{Root: t.TempDir(), Repository: customRepository(t), ConfigDir: source, PolicyForScope: func(Scope) (Policy, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.Snapshot(context.Background(), Scope{ActorID: "reader", SessionID: "assets"})
	if err != nil {
		t.Fatal(err)
	}
	if state.Gaggles[0].DSLVersion != "3.0" || state.Gaggles[0].Spec.Interactive != nil {
		t.Fatal("native projection must preserve DSL3 pin but remove host registration")
	}
	if len(state.Goobers) != 1 || len(state.Goobers[0].Spec.Workflows) != 1 || state.Goobers[0].Spec.Workflows[0] != "selected-check" {
		t.Fatalf("persona backlinks not attenuated: %+v", state.Goobers)
	}
	for _, name := range []string{"gaggles/team/goobers/helper/instructions.md", "gaggles/team/goobers/helper/assets/checklist.txt", "gaggles/team/skills/review/SKILL.md"} {
		if _, err := os.Stat(filepath.Join(state.InstanceRoot, "config", name)); err != nil {
			t.Fatalf("required native asset missing: %s: %v", name, err)
		}
	}
	for _, name := range []string{".git/irrelevant", "secrets/token.txt", "private.yaml", "other.yaml"} {
		if _, err := os.Stat(filepath.Join(state.InstanceRoot, "config", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("out-of-scope source copied: %s", name)
		}
	}
	if err = os.Symlink(filepath.Join(source, "secrets/token.txt"), filepath.Join(source, "gaggles/team/goobers/helper/assets/leak.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Snapshot(context.Background(), Scope{ActorID: "reader", SessionID: "symlink"}); err == nil {
		t.Fatal("symlink asset copied")
	}
}

func TestRecordedSnapshotSurvivesRetiredPolicyWithoutEffects(t *testing.T) {
	s, scope, p := scopedNativeService(t, "")
	ctx := context.Background()
	label := "recorded review"
	a, err := s.Propose(ctx, scope, Request{Kind: "tweak_gaggle", DisplayName: &label, IdempotencyKey: "recorded"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := configDigest(s.scopeDir(scope))
	if err != nil {
		t.Fatal(err)
	}
	p.Revision = "retired"
	s.opts.PolicyForScope = func(Scope) (Policy, error) { t.Fatal("historical read called current policy"); return Policy{}, nil }
	s.opts.ConfigDirForScope = func(Scope) (string, error) { t.Fatal("historical read called current config"); return "", nil }
	s.opts.RepositoryForScope = func(Scope) (string, error) { t.Fatal("historical read called current repository"); return "", nil }
	recorded, err := s.SnapshotRecorded(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	if !recorded.ReadOnly || recorded.Mode != "recorded-native-config" || recorded.Policy.Revision != "revision-one" || len(recorded.Actions) != 1 || recorded.Actions[0].ID != a.ID || recorded.WorkflowDraft != "" {
		t.Fatalf("incorrect recorded state: %+v", recorded)
	}
	after, err := configDigest(s.scopeDir(scope))
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatal("historical snapshot changed persisted files")
	}
	unknown := Scope{ActorID: scope.ActorID, SessionID: "absent-history"}
	if _, err = s.SnapshotRecorded(ctx, unknown); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("absent history unexpectedly initialized: %v", err)
	}
	if _, err = os.Stat(s.scopeDir(unknown)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("absent history read created directory")
	}
}

func TestNativeSharedGooberProjectionUsesOnlyDeclaredSiblingRoot(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "config")
	if err := copyTree(nativePolicySource(t), source); err != nil {
		t.Fatal(err)
	}
	for rel, text := range map[string]string{
		"goobers/helper/goober.yaml":          "apiVersion: goobers.dev/v1alpha1\nkind: Goober\nmetadata: {name: helper}\nspec:\n  role: helper\n  instructions: instructions.md\n",
		"goobers/helper/instructions.md":      "Read the selected repository.\n",
		"goobers/helper/assets/checklist.txt": "Check the workflow result.\n",
		"secrets/token.txt":                   "must never be copied\n",
	} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(source, "team.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("  interactive:\n    surfaces:\n      - name: helper\n        mode: helper\n        gooberRef: helper\n        executionModes: [local-tools]\n        defaultExecutionMode: local-tools\n        tools: [session_context]\n        workflows: [selected-check]\n        session: {leaseSeconds: 30, maxTurnSeconds: 600, maxChildren: 1, maxActive: 2, maxActivePerOwner: 1}\n")...)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	p := Policy{Revision: "shared-source", Gaggle: "team", Workflows: []string{"selected-check"}, CommandCapabilities: []CommandCapability{CommandNodeCheck, CommandNodeTest}}
	s, err := New(Options{Root: t.TempDir(), Repository: customRepository(t), ConfigDir: source, PolicyForScope: func(Scope) (Policy, error) { return p, nil }})
	if err != nil {
		t.Fatal(err)
	}
	state, err := s.Snapshot(context.Background(), Scope{ActorID: "reader", SessionID: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Goobers) != 1 || state.Goobers[0].Name != "helper" || state.Goobers[0].Spec.Gaggle != "team" {
		t.Fatalf("shared persona not rehomed into selected gaggle: %+v", state.Goobers)
	}
	for _, rel := range []string{"instructions.md", "assets/checklist.txt"} {
		if _, err = os.Stat(filepath.Join(state.InstanceRoot, "config/gaggles/team/goobers/helper", rel)); err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range []string{"../secrets/token.txt", "../other/helper/goober.yaml", "../../goobers/helper/goober.yaml", "../goobers/other/goober.yaml"} {
		if _, _, err = gooberAssetSource(source, rel, "helper"); err == nil {
			t.Fatalf("foreign source accepted: %s", rel)
		}
	}
	if err = os.Rename(filepath.Join(root, "goobers"), filepath.Join(root, "saved-goobers")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(root, "saved-goobers"), filepath.Join(root, "goobers")); err != nil {
		t.Fatal(err)
	}
	if _, _, err = gooberAssetSource(source, "../goobers/helper/goober.yaml", "helper"); err == nil {
		t.Fatal("symlink shared root accepted")
	}
}
