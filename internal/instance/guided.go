package instance

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	configexamples "github.com/goobers/goobers/config-examples"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/procenv"
)

const (
	// GuidedWorkflowImplementation identifies the canonical implementation workflow.
	GuidedWorkflowImplementation = "implementation"
	// GuidedWorkflowBacklogCuration identifies the canonical backlog-curation workflow.
	GuidedWorkflowBacklogCuration = "backlog-curation"
	// GuidedWorkflowWorkNomination identifies the canonical work-nomination workflow.
	GuidedWorkflowWorkNomination   = "work-nomination"
	guidedExampleGaggle            = "acme-web"
	guidedExampleDisplayName       = "Acme Web"
	guidedExampleRoot              = "gaggles/" + guidedExampleGaggle
	guidedRepositoryConnectionName = "github-main"
	guidedBacklogConnectionName    = "github-backlog"
)

var guidedWorkflowOrder = []string{
	GuidedWorkflowImplementation,
	GuidedWorkflowBacklogCuration,
	GuidedWorkflowWorkNomination,
}

var guidedWorkflowGoobers = map[string][]string{
	GuidedWorkflowImplementation:  {"implementer", "reviewer"},
	GuidedWorkflowBacklogCuration: {"curator"},
	GuidedWorkflowWorkNomination:  {"nominator"},
}

// GuidedWorkflowNames returns the canonical workflows available during guided
// initialization in their display order.
func GuidedWorkflowNames() []string {
	return append([]string(nil), guidedWorkflowOrder...)
}

// GuidedOptions describes one guided first-run instance.
type GuidedOptions struct {
	GaggleName      string
	DisplayName     string
	RepoOwner       string
	RepoName        string
	RepoBranch      string
	RepoTokenEnv    string
	CopilotTokenEnv string
	Workflows       []string
	CICommand       []string
}

// InitGuided scaffolds a repository-specific instance from selected canonical
// workflows.
func InitGuided(root string, opts GuidedOptions) (*InitResult, error) {
	opts = normalizeGuidedOptions(opts)
	if err := validateGuidedOptions(opts); err != nil {
		return nil, err
	}
	if err := CheckGuidedInitTarget(root); err != nil {
		return nil, err
	}
	return initWithSeed(root, guidedConfig(opts), func(dir string) error {
		return copyGuidedConfig(dir, opts)
	})
}

// CheckGuidedInitTarget rejects configuration that guided setup cannot safely
// replace. Other scaffold directories may already exist.
func CheckGuidedInitTarget(root string) error {
	layout := NewLayout(root)
	if _, err := os.Stat(layout.ConfigFile()); err == nil {
		return fmt.Errorf("guided setup requires an unconfigured target: %s already exists; choose an empty path", ConfigFileName)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s: %w", ConfigFileName, err)
	}

	populated, err := dirHasFiles(layout.ConfigDir())
	if err != nil {
		return fmt.Errorf("inspect %s: %w", ConfigDirName, err)
	}
	if populated {
		return fmt.Errorf("guided setup requires an unconfigured target: %s already contains files; choose an empty path", ConfigDirName)
	}
	return nil
}

func normalizeGuidedOptions(opts GuidedOptions) GuidedOptions {
	opts.GaggleName = strings.TrimSpace(opts.GaggleName)
	opts.DisplayName = strings.TrimSpace(opts.DisplayName)
	opts.RepoOwner = strings.TrimSpace(opts.RepoOwner)
	opts.RepoName = strings.TrimSpace(opts.RepoName)
	opts.RepoBranch = strings.TrimSpace(opts.RepoBranch)
	opts.RepoTokenEnv = strings.TrimSpace(opts.RepoTokenEnv)
	opts.CopilotTokenEnv = strings.TrimSpace(opts.CopilotTokenEnv)
	if opts.DisplayName == "" {
		opts.DisplayName = opts.RepoOwner + "/" + opts.RepoName
	}
	if opts.RepoBranch == "" {
		opts.RepoBranch = "main"
	}
	return opts
}

func validateGuidedOptions(opts GuidedOptions) error {
	for label, value := range map[string]string{
		"gaggle name":       opts.GaggleName,
		"repository owner":  opts.RepoOwner,
		"repository name":   opts.RepoName,
		"repository branch": opts.RepoBranch,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if !guidedObjectName(opts.GaggleName) {
		return fmt.Errorf("gaggle name %q must contain lowercase letters, numbers, or hyphens and start and end with a letter or number", opts.GaggleName)
	}
	for label, value := range map[string]string{
		"repository token environment variable": opts.RepoTokenEnv,
		"Copilot token environment variable":    opts.CopilotTokenEnv,
	} {
		if !ValidGuidedTokenEnvName(value) {
			return fmt.Errorf("%s must name a valid environment variable; do not provide a token value", label)
		}
	}
	if len(opts.Workflows) == 0 {
		return fmt.Errorf("select at least one workflow")
	}
	seen := make(map[string]bool, len(opts.Workflows))
	for _, name := range opts.Workflows {
		if _, ok := guidedWorkflowGoobers[name]; !ok {
			return fmt.Errorf("unknown guided workflow %q", name)
		}
		if seen[name] {
			return fmt.Errorf("guided workflow %q selected more than once", name)
		}
		seen[name] = true
	}
	if seen[GuidedWorkflowImplementation] {
		if len(opts.CICommand) == 0 {
			return fmt.Errorf("local CI command is required when selecting the implementation workflow")
		}
		for _, arg := range opts.CICommand {
			if strings.TrimSpace(arg) == "" {
				return fmt.Errorf("local CI command arguments must not be empty")
			}
		}
	} else if len(opts.CICommand) > 0 {
		return fmt.Errorf("local CI command requires the implementation workflow")
	}
	return nil
}

func guidedObjectName(name string) bool {
	if name == "" || len(name) > 54 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// ValidGuidedTokenEnvName reports whether value is a valid environment variable
// name and not a GitHub token value accidentally pasted into the prompt.
func ValidGuidedTokenEnvName(value string) bool {
	if !procenv.ValidName(value) {
		return false
	}
	for _, prefix := range []string{"github_pat_", "ghp_", "gho_", "ghu_", "ghs_", "ghr_"} {
		if strings.HasPrefix(value, prefix) {
			return false
		}
	}
	return true
}

func guidedConfig(opts GuidedOptions) *Config {
	cfg := &Config{
		APIVersion: ConfigAPIVersion,
		Kind:       ConfigKind,
		Repos: []RepoRef{{
			Provider: "github",
			Owner:    opts.RepoOwner,
			Name:     opts.RepoName,
			Token:    TokenRef{Env: opts.RepoTokenEnv},
		}},
		Credentials: []CredentialGrant{{
			Capability: string(capability.AgentModel),
			Token:      TokenRef{Env: opts.CopilotTokenEnv},
		}},
		RunConditions: RunConditions{MaxParallelRuns: 1},
	}
	return cfg
}

func copyGuidedConfig(dir string, opts GuidedOptions) error {
	if err := os.MkdirAll(filepath.Join(dir, "gaggles", opts.GaggleName), 0o755); err != nil {
		return err
	}
	if err := writeGuidedFile(filepath.Join(dir, "manifest.yaml"), guidedManifest(opts)); err != nil {
		return err
	}
	gagglePath := filepath.Join(dir, "gaggles", opts.GaggleName, "gaggle.yaml")
	if err := writeGuidedFile(gagglePath, guidedGaggle(opts)); err != nil {
		return err
	}

	selected := make(map[string]bool, len(opts.Workflows))
	goobers := make(map[string]bool)
	for _, workflow := range opts.Workflows {
		selected[workflow] = true
		for _, goober := range guidedWorkflowGoobers[workflow] {
			goobers[goober] = true
		}
		if err := copyGuidedWorkflow(dir, workflow, opts); err != nil {
			return err
		}
	}
	names := make([]string, 0, len(goobers))
	for name := range goobers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := copyGuidedGoober(dir, name, selected, opts); err != nil {
			return err
		}
	}
	return nil
}

func copyGuidedWorkflow(dir, name string, opts GuidedOptions) error {
	source := guidedExampleRoot + "/workflows/" + name + ".yaml"
	data, err := configexamples.Files.ReadFile(source)
	if err != nil {
		return err
	}
	var workflow apiv1.Workflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return fmt.Errorf("decode canonical workflow %s: %w", name, err)
	}
	workflow.Spec.Gaggle = opts.GaggleName
	for i := range workflow.Spec.Tasks {
		task := &workflow.Spec.Tasks[i]
		if task.Type == apiv1.TaskAgentic {
			task.Capabilities = prependCapability(task.Capabilities, string(capability.AgentModel))
		}
	}
	data, err = yaml.Marshal(workflow)
	if err != nil {
		return fmt.Errorf("encode guided workflow %s: %w", name, err)
	}
	target := filepath.Join(dir, "gaggles", opts.GaggleName, "workflows", name+".yaml")
	return writeGuidedFile(target, data)
}

func copyGuidedGoober(dir, name string, selected map[string]bool, opts GuidedOptions) error {
	sourceDir := guidedExampleRoot + "/goobers/" + name
	data, err := configexamples.Files.ReadFile(sourceDir + "/goober.yaml")
	if err != nil {
		return err
	}
	var goober apiv1.Goober
	if err := yaml.Unmarshal(data, &goober); err != nil {
		return fmt.Errorf("decode canonical goober %s: %w", name, err)
	}
	goober.Spec.Gaggle = opts.GaggleName
	goober.Spec.Capabilities = prependCapability(goober.Spec.Capabilities, string(capability.AgentModel))
	workflows := goober.Spec.Workflows[:0]
	for _, workflow := range goober.Spec.Workflows {
		if selected[workflow] {
			workflows = append(workflows, workflow)
		}
	}
	goober.Spec.Workflows = workflows
	data, err = yaml.Marshal(goober)
	if err != nil {
		return fmt.Errorf("encode guided goober %s: %w", name, err)
	}
	targetDir := filepath.Join(dir, "gaggles", opts.GaggleName, "goobers", name)
	if err := writeGuidedFile(filepath.Join(targetDir, "goober.yaml"), data); err != nil {
		return err
	}
	instructions, err := configexamples.Files.ReadFile(sourceDir + "/instructions.md")
	if err != nil {
		return err
	}
	instructions = []byte(strings.ReplaceAll(string(instructions), guidedExampleDisplayName, opts.DisplayName))
	return writeGuidedFile(filepath.Join(targetDir, "instructions.md"), instructions)
}

func prependCapability(capabilities []string, name string) []string {
	for _, existing := range capabilities {
		if existing == name {
			return capabilities
		}
	}
	return append([]string{name}, capabilities...)
}

func guidedManifest(opts GuidedOptions) []byte {
	return []byte(fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Manifest
metadata:
  name: %s
spec:
  instance:
    name: %s
    environment: dev
  connections:
    - name: %s
      type: repo
      provider: github
      secretRef:
        name: repo-token
    - name: %s
      type: backlog
      provider: github
      secretRef:
        name: backlog-token
  gaggles:
    - %s
`, yamlScalar(opts.GaggleName+"-instance"), yamlScalar(opts.GaggleName),
		guidedRepositoryConnectionName, guidedBacklogConnectionName, yamlScalar(opts.GaggleName)))
}

func guidedGaggle(opts GuidedOptions) []byte {
	ciCommand := ""
	if len(opts.CICommand) > 0 {
		ciCommand = "  ciCommand: " + yamlStringList(opts.CICommand) + "\n"
	}
	return []byte(fmt.Sprintf(`apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: %s
spec:
  displayName: %s
  project:
    provider: github
    owner: %s
    name: %s
    branch: %s
    connectionRef: %s
  backlog:
    provider: github
    project: %s
    labels:
      - goobers
    connectionRef: %s
%s  isolation:
    namespace: %s
`, yamlScalar(opts.GaggleName), yamlScalar(opts.DisplayName), yamlScalar(opts.RepoOwner),
		yamlScalar(opts.RepoName), yamlScalar(opts.RepoBranch), guidedRepositoryConnectionName,
		yamlScalar(opts.RepoOwner+"/"+opts.RepoName), guidedBacklogConnectionName, ciCommand,
		yamlScalar("gaggle-"+opts.GaggleName)))
}

func yamlScalar(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func yamlStringList(values []string) string {
	data, _ := json.Marshal(values)
	return string(data)
}

func writeGuidedFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
