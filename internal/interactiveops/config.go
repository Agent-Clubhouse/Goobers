package interactiveops

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
	"sigs.k8s.io/yaml"
)

var namePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

func (s *Service) repository(scope Scope, dir string) (string, error) {
	if s.opts.RepositoryForScope != nil {
		return s.opts.RepositoryForScope(scope)
	}
	return filepath.Join(dir, "repository"), nil
}
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0700)
		}
		if !d.Type().IsRegular() {
			return errors.New("fixture trees may contain only regular files")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if len(b) > 4<<20 {
			return errors.New("fixture file too large")
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0600)
	})
}
func (s *Service) ensure(scope Scope, dir string) error {
	owner := filepath.Join(dir, "owner.json")
	if b, err := os.ReadFile(owner); err == nil {
		var got Scope
		if json.Unmarshal(b, &got) != nil || got != scope {
			return errors.New("fixture ownership mismatch")
		}
		return instance.RecoverConfigTransaction(instance.NewLayout(filepath.Join(dir, "instance")))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "actions"), 0700); err != nil {
		return err
	}
	repo, err := s.repository(scope, dir)
	if err != nil {
		return err
	}
	if s.opts.RepositoryForScope == nil {
		if s.opts.Repository == "" {
			return errors.New("fixture repository is required")
		}
		if err = copyTree(s.opts.Repository, repo); err != nil {
			return err
		}
	}
	if _, err = os.Stat(filepath.Join(repo, "invoice.test.mjs")); err != nil {
		return fmt.Errorf("fixture invoice.test.mjs is required: %w", err)
	}
	root := filepath.Join(dir, "instance")
	if _, err = instance.InitDemo(root); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(dir, "seed-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	files := map[string]string{
		"manifest.yaml":      "apiVersion: goobers.dev/v1alpha1\nkind: Manifest\nmetadata:\n  name: interactive-fixture\nspec:\n  instance: {name: interactive-fixture, environment: dev}\n  gaggles: [interactive]\n",
		"gaggle.yaml":        "apiVersion: goobers.dev/v1alpha1\nkind: Gaggle\nmetadata:\n  name: interactive\nspec:\n  displayName: Interactive invoice workspace\n  project: {provider: github, owner: controlled-fixture, name: invoice, branch: main}\n  backlog: {provider: github, project: controlled-fixture/invoice}\n  isolation: {namespace: interactive-fixture}\n",
		"invoice-check.yaml": workflowDraft("invoice-check", repo),
	}
	for name, b := range files {
		if err = os.WriteFile(filepath.Join(staging, name), []byte(b), 0600); err != nil {
			return err
		}
	}
	if err = validateConfig(staging); err != nil {
		return err
	}
	swap, err := instance.PrepareConfigDirSwap(instance.NewLayout(root), staging)
	if err != nil {
		return err
	}
	if err = swap.Commit(); err != nil {
		return err
	}
	return writeJSON(owner, scope)
}
func workflowDraft(name, repo string) string {
	command, _ := json.Marshal([]string{"node", "--test", filepath.Join(repo, "invoice.test.mjs")})
	return fmt.Sprintf("apiVersion: goobers.dev/v1alpha1\nkind: Workflow\ndslVersion: \"2.0\"\nmetadata:\n  name: %s\n  annotations:\n    goobers.dev/acknowledge-manual-only: \"true\"\nspec:\n  gaggle: interactive\n  displayName: Invoice regression check\n  triggers: [{type: manual}]\n  readiness: {maxConcurrentRuns: 1}\n  start: test\n  tasks:\n    - name: test\n      type: deterministic\n      goal: Run the actual invoice regression tests in this session workspace\n      timeoutSeconds: 30\n      run:\n        workspace: scratch\n        network: none\n        command: %s\n", name, command)
}
func validateConfig(path string) error {
	_, report, err := instance.LoadConfigDir(path)
	if err == nil {
		return nil
	}
	messages := []string{err.Error()}
	if report != nil {
		for _, issue := range report.Issues {
			if issue.Severity == "error" {
				messages = append(messages, issue.Message)
			}
		}
	}
	return errors.New(strings.Join(messages, "; "))
}
func onlyKeys(m map[string]any, keys ...string) error {
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	for k := range m {
		if !allowed[k] {
			return fmt.Errorf("prototype does not authorize workflow field %q", k)
		}
	}
	return nil
}
func validateLocalWorkflow(content, repo string) (apiv1.Workflow, error) {
	var wf apiv1.Workflow
	if len(content) > 32768 {
		return wf, errors.New("workflow exceeds 32 KiB")
	}
	if err := yaml.UnmarshalStrict([]byte(content), &wf); err != nil {
		return wf, err
	}
	if wf.Kind != "Workflow" || wf.APIVersion != "goobers.dev/v1alpha1" || !namePattern.MatchString(wf.Name) || wf.Spec.Gaggle != "interactive" {
		return wf, errors.New("workflow must target the interactive fixture gaggle with a safe name")
	}
	var raw map[string]any
	if err := yaml.UnmarshalStrict([]byte(content), &raw); err != nil {
		return wf, err
	}
	spec, ok := raw["spec"].(map[string]any)
	if !ok {
		return wf, errors.New("workflow spec missing")
	}
	if err := onlyKeys(spec, "gaggle", "displayName", "enabled", "triggers", "readiness", "start", "tasks"); err != nil {
		return wf, err
	}
	if len(wf.Spec.Triggers) != 1 || wf.Spec.Triggers[0].Type != apiv1.TriggerManual {
		return wf, errors.New("only manual trigger is authorized")
	}
	if len(wf.Spec.Tasks) == 0 || len(wf.Spec.Tasks) > 8 {
		return wf, errors.New("workflow must have 1–8 bounded tasks")
	}
	tasks, _ := spec["tasks"].([]any)
	for _, task := range tasks {
		m, ok := task.(map[string]any)
		if !ok {
			return wf, errors.New("invalid task")
		}
		if err := onlyKeys(m, "name", "type", "goal", "timeoutSeconds", "run", "next"); err != nil {
			return wf, err
		}
	}
	for _, task := range wf.Spec.Tasks {
		if task.Type != apiv1.TaskDeterministic || task.TimeoutSeconds <= 0 || task.TimeoutSeconds > 30 || task.Run == nil {
			return wf, errors.New("only deterministic tasks with 1–30 second limits are authorized")
		}
		run := task.Run
		expected := []string{"node", "--test", filepath.Join(repo, "invoice.test.mjs")}
		if !reflect.DeepEqual(run.Command, expected) || run.Network != apiv1.NetworkNone || run.Workspace != apiv1.WorkspaceScratch || run.Script != "" || len(run.Env) > 0 || run.InjectRunContext || run.SyncBase {
			return wf, errors.New("task must use the exact session invoice test command, network:none and scratch; credentials and arbitrary commands are forbidden")
		}
	}
	return wf, nil
}
func (s *Service) preview(scope Scope, dir string, req Request) (string, string, error) {
	root := filepath.Join(dir, "instance")
	set, _, err := instance.LoadConfigDir(filepath.Join(root, "config"))
	if err != nil {
		return "", "", err
	}
	if req.Gaggle != "interactive" {
		return "", "", errors.New("operation is outside the session fixture gaggle")
	}
	switch req.Kind {
	case "tweak_gaggle", "tweak_workflow":
		if req.DisplayName == nil && req.Enabled == nil {
			return "", "", errors.New("provide displayName or enabled")
		}
		if req.DisplayName != nil && (len(*req.DisplayName) > 100 || strings.TrimSpace(*req.DisplayName) == "") {
			return "", "", errors.New("displayName must contain 1–100 characters")
		}
		if req.Kind == "tweak_workflow" {
			found := false
			for _, wf := range set.Workflows {
				found = found || wf.Name == req.Workflow
			}
			if !found {
				return "", "", errors.New("unknown workflow")
			}
		}
		stage, err := s.stageConfig(scope, dir, req)
		if err != nil {
			return "", "", err
		}
		defer os.RemoveAll(stage)
		b, _ := json.MarshalIndent(map[string]any{"gaggle": req.Gaggle, "workflow": req.Workflow, "displayName": req.DisplayName, "enabled": req.Enabled}, "", "  ")
		return "Apply scoped " + req.Kind + " to the isolated local fixture", string(b), nil
	case "add_workflow":
		repo, err := s.repository(scope, dir)
		if err != nil {
			return "", "", err
		}
		wf, err := validateLocalWorkflow(req.WorkflowYAML, repo)
		if err != nil {
			return "", "", err
		}
		for _, existing := range set.Workflows {
			if existing.Name == wf.Name {
				return "", "", errors.New("workflow already exists; choose a new name")
			}
		}
		stage, err := s.stageConfig(scope, dir, req)
		if err != nil {
			return "", "", err
		}
		defer os.RemoveAll(stage)
		return "Add validated workflow " + wf.Name + " to the local fixture", req.WorkflowYAML, nil
	case "submit_run":
		if s.opts.Binary == "" {
			return "", "", errors.New("native Goobers binary unavailable; build cmd/goobers and configure Binary")
		}
		for _, g := range set.Gaggles {
			if g.Spec.Enabled != nil && !*g.Spec.Enabled {
				return "", "", errors.New("gaggle is disabled")
			}
		}
		for _, wf := range set.Workflows {
			if wf.Name != req.Workflow {
				continue
			}
			if wf.Spec.Enabled != nil && !*wf.Spec.Enabled {
				return "", "", errors.New("workflow is disabled")
			}
			repo, err := s.repository(scope, dir)
			if err != nil {
				return "", "", err
			}
			b, err := yaml.Marshal(wf)
			if err != nil {
				return "", "", err
			}
			if _, err = validateLocalWorkflow(string(b), repo); err != nil {
				return "", "", err
			}
			return "Submit " + req.Gaggle + "/" + req.Workflow + " through the native Goobers scheduler", "Admission is distinct from completion. Observe the run journal for actual test results. No provider credentials or remote effects are authorized.", nil
		}
		return "", "", errors.New("unknown workflow")
	case "stop_run":
		actions, err := allActions(dir)
		if err != nil {
			return "", "", err
		}
		for _, a := range actions {
			if a.Kind == "submit_run" && (a.ID == req.RunID || a.RunID == req.RunID) && (a.Status == "running" || a.Status == "admitted") {
				return "Request stop for owned workflow run", "Cancellation is pending until native execution stops and its terminal journal is observed.", nil
			}
		}
		return "", "", errors.New("owned active run not found")
	case "steer_run":
		return "", "", errors.New("steering deterministic workflow processes is unsupported; steer the interactive workspace session instead")
	default:
		return "", "", errors.New("unsupported helper operation")
	}
}
func (s *Service) stageConfig(scope Scope, dir string, req Request) (string, error) {
	stage, err := os.MkdirTemp(dir, "candidate-")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(stage)
		}
	}()
	config := filepath.Join(dir, "instance", "config")
	if err = copyTree(config, stage); err != nil {
		return "", err
	}
	set, _, err := instance.LoadConfigDir(stage)
	if err != nil {
		return "", err
	}
	var file string
	var data []byte
	switch req.Kind {
	case "add_workflow":
		repo, err := s.repository(scope, dir)
		if err != nil {
			return "", err
		}
		wf, err := validateLocalWorkflow(req.WorkflowYAML, repo)
		if err != nil {
			return "", err
		}
		file = wf.Name + ".yaml"
		if _, err = os.Lstat(filepath.Join(stage, file)); !errors.Is(err, os.ErrNotExist) {
			return "", errors.New("workflow path already exists")
		}
		data = []byte(req.WorkflowYAML)
	case "tweak_gaggle":
		g := set.Gaggles[0]
		if req.DisplayName != nil {
			g.Spec.DisplayName = *req.DisplayName
		}
		if req.Enabled != nil {
			g.Spec.Enabled = req.Enabled
		}
		file, _ = set.GaggleSource(g.Name)
		data, err = marshalConfig(g)
	case "tweak_workflow":
		for _, wf := range set.Workflows {
			if wf.Name != req.Workflow {
				continue
			}
			if req.DisplayName != nil {
				wf.Spec.DisplayName = *req.DisplayName
			}
			if req.Enabled != nil {
				wf.Spec.Enabled = req.Enabled
			}
			file, _ = set.WorkflowSource(req.Gaggle, wf.Name)
			data, err = marshalConfig(wf)
		}
	default:
		return "", errors.New("not a configuration action")
	}
	if err != nil {
		return "", err
	}
	if file == "" {
		return "", errors.New("configuration source not found")
	}
	if err = os.WriteFile(filepath.Join(stage, file), data, 0600); err != nil {
		return "", err
	}
	if err = validateConfig(stage); err != nil {
		return "", err
	}
	ok = true
	return stage, nil
}
func (s *Service) applyConfig(scope Scope, dir string, req Request) error {
	stage, err := s.stageConfig(scope, dir, req)
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	swap, err := instance.PrepareConfigDirSwap(instance.NewLayout(filepath.Join(dir, "instance")), stage)
	if err != nil {
		return err
	}
	return swap.Commit()
}

func readBacklog(repo string) []BacklogItem {
	items := []BacklogItem{}
	data, err := os.ReadFile(filepath.Join(repo, "gaggle-context.json"))
	if err != nil {
		return items
	}
	var fixture struct {
		Backlog []struct {
			ID    int    `json:"id"`
			Title string `json:"title"`
		} `json:"backlog"`
	}
	if json.Unmarshal(data, &fixture) != nil {
		return items
	}
	for _, item := range fixture.Backlog {
		items = append(items, BacklogItem{ID: item.ID, Title: item.Title, State: "fixture", Source: "controlled ADO-shaped fixture from gaggle-context.json; no live Azure request"})
	}
	return items
}

func marshalConfig(value any) ([]byte, error) {
	b, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err = json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	delete(raw, "status")
	return yaml.Marshal(raw)
}
