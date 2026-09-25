package providermatrix

import (
	"io/fs"
	"os"
	"path/filepath"
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
			instance.GuidedWorkflowWorkNomination,
		},
		prCI: true,
	},
	{
		name:      "scaffold/local-ci",
		workflows: []string{instance.GuidedWorkflowImplementation, instance.GuidedWorkflowBacklogCuration},
	},
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

// standardScaffoldOptions mirrors cmd/goobers standardInitOptions' defaults
// for `init --template=standard --provider=<provider>` with placeholder
// repository identity.
func standardScaffoldOptions(provider apiv1.Provider, variant scaffoldVariant) instance.GuidedOptions {
	opts := instance.GuidedOptions{
		GaggleName: "example", RepoProvider: string(provider),
		RepoOwner: "your-org", RepoName: "your-repo",
		RepoTokenEnv:         "GOOBERS_GITHUB_TOKEN",
		WorkTrackingTokenEnv: "GOOBERS_GITHUB_ISSUES_TOKEN",
		PullRequestTokenEnv:  "GOOBERS_GITHUB_PR_TOKEN",
		RepoPushTokenEnv:     "GOOBERS_GITHUB_PUSH_TOKEN",
		Workflows:            append([]string(nil), variant.workflows...),
		PullRequestCI:        variant.prCI,
	}
	if !variant.prCI {
		opts.CICommand = []string{"make", "test"}
		opts.RequiredCapabilities = []string{"go"}
	}
	if provider == apiv1.ProviderADO {
		opts.RepoProject = "your-project"
		opts.RepoAuthKind = instance.ADOAuthPAT
		opts.RepoTokenEnv = "GOOBERS_ADO_TOKEN"
		opts.WorkTrackingTokenEnv, opts.PullRequestTokenEnv, opts.RepoPushTokenEnv = "", "", ""
	}
	return opts
}
