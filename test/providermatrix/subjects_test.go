package providermatrix

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// Neutral placeholders for the synthesised gaggles.
const (
	adoOrganization = "example-org"
	adoProject      = "example-project"
	giteaBaseURL    = "https://gitea.example.com"
)

// rewriteGaggles moves every Gaggle document under dir onto provider: both
// spec.project and spec.backlog, with that provider's repository shape (an ADO
// organization and project, a Gitea base URL). Other gaggle fields are kept.
// The instance manifest's repo and backlog connections move with them, so the
// synthesised instance is what an adopter on provider would write (the
// standard scaffold names its forge connections' provider the same way).
func rewriteGaggles(t *testing.T, dir string, provider apiv1.Provider) {
	t.Helper()
	rewritten := 0
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Base(path) != "gaggle.yaml" {
			return walkErr
		}
		rewriteGaggleFile(t, path, provider)
		rewritten++
		return nil
	})
	if err != nil {
		t.Fatalf("rewrite gaggles under %s: %v", dir, err)
	}
	if rewritten == 0 {
		t.Fatalf("no gaggle.yaml under %s; the matrix would not exercise %s", dir, provider)
	}
	rewriteManifestConnections(t, dir, map[string]apiv1.Provider{"repo": provider, "backlog": provider})
}

// forgeProviders are the connection providers a forge connection may name;
// a connection to anything else (telemetry, identity, a harness) is never
// moved between forges.
var forgeProviders = map[string]bool{
	string(apiv1.ProviderGitHub): true,
	string(apiv1.ProviderADO):    true,
	string(apiv1.ProviderGitea):  true,
}

// rewriteManifestConnections sets the provider of every forge connection in
// dir's manifest.yaml whose type has an entry in byType. A tree without a
// manifest, or one declaring no such connection, is left as is.
func rewriteManifestConnections(t *testing.T, dir string, byType map[string]apiv1.Provider) {
	t.Helper()
	path := filepath.Join(dir, "manifest.yaml")
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if doc["kind"] != "Manifest" {
		t.Fatalf("%s is not a single Manifest document (kind %v)", path, doc["kind"])
	}
	spec, _ := doc["spec"].(map[string]any)
	connections, _ := spec["connections"].([]any)
	for _, raw := range connections {
		connection, _ := raw.(map[string]any)
		kind, _ := connection["type"].(string)
		current, _ := connection["provider"].(string)
		if provider, ok := byType[kind]; ok && forgeProviders[current] {
			connection["provider"] = string(provider)
		}
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func rewriteGaggleFile(t *testing.T, path string, provider apiv1.Provider) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if doc["kind"] != "Gaggle" {
		t.Fatalf("%s is not a single Gaggle document (kind %v)", path, doc["kind"])
	}
	spec, _ := doc["spec"].(map[string]any)
	project, _ := spec["project"].(map[string]any)
	backlog, _ := spec["backlog"].(map[string]any)
	if project == nil || backlog == nil {
		t.Fatalf("%s has no spec.project or spec.backlog", path)
	}
	setProviderShape(project, backlog, provider)
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// setProviderShape gives a gaggle's project and backlog the repository shape
// provider expects. The repository name is kept, so workflow references and
// sibling matching behave as on the original provider.
func setProviderShape(project, backlog map[string]any, provider apiv1.Provider) {
	for _, ref := range []map[string]any{project, backlog} {
		ref["provider"] = string(provider)
		delete(ref, "baseUrl")
	}
	delete(project, "project")
	owner, _ := project["owner"].(string)
	name, _ := project["name"].(string)
	switch provider {
	case apiv1.ProviderADO:
		project["owner"], project["project"] = adoOrganization, adoProject
		backlog["project"] = adoProject
	case apiv1.ProviderGitea:
		project["baseUrl"], backlog["baseUrl"] = giteaBaseURL, giteaBaseURL
		backlog["project"] = owner + "/" + name
	default:
		backlog["project"] = owner + "/" + name
	}
}

// scaffoldVariant is one shape of `goobers init --template=standard`: every
// guided workflow module, with pull-request CI or with a local CI command.
// work-nomination is GitHub-only: init refuses it on Azure DevOps, so the ADO
// scaffold is built without it. Guided merge-review is Azure DevOps-only:
// init refuses it on GitHub, so the GitHub scaffold is built without it.
type scaffoldVariant struct {
	name      string
	workflows []string
	prCI      bool
}

var scaffoldVariants = []scaffoldVariant{
	{
		name: "scaffold/pr-ci",
		workflows: []string{
			instance.GuidedWorkflowImplementation,
			instance.GuidedWorkflowBacklogCuration,
			instance.GuidedWorkflowMergeReview,
			instance.GuidedWorkflowWorkNomination,
		},
		prCI: true,
	},
	{
		name: "scaffold/local-ci",
		workflows: []string{
			instance.GuidedWorkflowImplementation,
			instance.GuidedWorkflowBacklogCuration,
			instance.GuidedWorkflowMergeReview,
		},
	},
}

// scaffoldWorkflows is variant's module set as init accepts it on provider.
func scaffoldWorkflows(provider apiv1.Provider, variant scaffoldVariant) []string {
	workflows := append([]string(nil), variant.workflows...)
	refused := instance.GuidedWorkflowMergeReview
	if provider == apiv1.ProviderADO {
		refused = instance.GuidedWorkflowWorkNomination
	}
	return slices.DeleteFunc(workflows, func(name string) bool { return name == refused })
}

// scaffoldSubject seeds the standard scaffold natively for GitHub and ADO
// (`init --template=standard --provider=github|ado`). The scaffold has no
// Gitea mode, so the Gitea column rewrites the GitHub scaffold's gaggle.
func scaffoldSubject(variant scaffoldVariant) matrixSubject {
	return matrixSubject{name: variant.name, build: func(t *testing.T, provider apiv1.Provider) string {
		t.Helper()
		dir := filepath.Join(t.TempDir(), strings.ReplaceAll(variant.name, "/", "-"))
		native := provider
		if native != apiv1.ProviderADO {
			native = apiv1.ProviderGitHub
		}
		if _, err := instance.SeedGuidedConfigSource(dir, standardScaffoldOptions(native, variant)); err != nil {
			t.Fatalf("seed %s for %s: %v", variant.name, native, err)
		}
		if native != provider {
			rewriteGaggles(t, dir, provider)
		}
		return dir
	}}
}

// standardScaffoldOptions is `init --template=standard
// --provider=<provider>` with its default auth: the placeholder identity and
// credential defaults come from instance.StandardInitDefaults, the same
// helper init uses, so the gate cannot drift from what init writes.
func standardScaffoldOptions(provider apiv1.Provider, variant scaffoldVariant) instance.GuidedOptions {
	opts := instance.StandardInitDefaults(string(provider), "")
	opts.Workflows = scaffoldWorkflows(provider, variant)
	opts.PullRequestCI = variant.prCI
	if !variant.prCI {
		opts.CICommand = []string{"make", "test"}
		opts.RequiredCapabilities = []string{"go"}
	}
	return opts
}

// TestRewriteMovesManifestForgeConnections proves each column's synthesised
// instance is consistent: config-examples' manifest declares a repo and a
// backlog connection, and both follow the gaggles onto the column's provider
// (in topology (b), the backlog connection follows the backlog to GitHub).
func TestRewriteMovesManifestForgeConnections(t *testing.T) {
	t.Parallel()
	source := filepath.Join(repositoryRoot(t), "config-examples")
	check := func(t *testing.T, dir string, want map[string]apiv1.Provider) {
		t.Helper()
		set, report, err := instance.LoadConfigDir(dir)
		if err != nil {
			t.Fatalf("load %s: %v (report %+v)", dir, err, report)
		}
		seen := map[string]bool{}
		for _, connection := range set.Manifest.Spec.Connections {
			if provider, ok := want[connection.Type]; ok {
				seen[connection.Type] = true
				if connection.Provider != string(provider) {
					t.Errorf("%s connection %q provider = %q, want %q", connection.Type, connection.Name, connection.Provider, provider)
				}
			}
		}
		for kind := range want {
			if !seen[kind] {
				t.Fatalf("config-examples declares no %s connection; the check would be vacuous", kind)
			}
		}
	}
	for _, provider := range matrixProviders {
		t.Run(string(provider), func(t *testing.T) {
			t.Parallel()
			dir := shippedTreeSubject("config-examples", source).build(t, provider)
			check(t, dir, map[string]apiv1.Provider{"repo": provider, "backlog": provider})
		})
	}
	t.Run("topology-b", func(t *testing.T) {
		t.Parallel()
		dir := shippedTreeSubject("config-examples", source).build(t, apiv1.ProviderADO)
		moveBacklogsToGitHub(t, dir)
		check(t, dir, map[string]apiv1.Provider{"repo": apiv1.ProviderADO, "backlog": apiv1.ProviderGitHub})
	})
}
