package interactiveops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/workflow"
)

// CommandCapability is a closed host-selected capability, never an executable
// path or a model-provided shell fragment.
type CommandCapability string

// Command capabilities name the allowed deterministic child commands.
const (
	CommandNodeTest  CommandCapability = "node-test"
	CommandNodeCheck CommandCapability = "node-check"
)

// WorkflowDiagnostic identifies a bounded workflow validation problem.
type WorkflowDiagnostic struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Field    string `json:"field"`
	Message  string `json:"message"`
}

// WorkflowCommand describes an allowed deterministic command.
type WorkflowCommand struct {
	Stage      string            `json:"stage"`
	Capability CommandCapability `json:"capability"`
	Arguments  []string          `json:"arguments"`
}

// WorkflowValidation contains a normalized workflow and its validation evidence.
type WorkflowValidation struct {
	Valid          bool                 `json:"valid"`
	Name           string               `json:"name,omitempty"`
	NormalizedYAML string               `json:"normalizedYaml,omitempty"`
	BoundYAML      string               `json:"boundYaml,omitempty"`
	PayloadDigest  string               `json:"payloadDigest,omitempty"`
	BindingDigest  string               `json:"bindingDigest,omitempty"`
	Diagnostics    []WorkflowDiagnostic `json:"diagnostics"`
	Commands       []WorkflowCommand    `json:"commands"`
}

// ValidateWorkflow validates the native DSL and the host's bounded command
// policy without proposing/applying an action. NormalizedYAML is portable;
// BoundYAML contains only host-resolved paths within this scope's repository.
func (s *Service) ValidateWorkflow(ctx context.Context, scope Scope, content string) (WorkflowValidation, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowValidation{}, err
	}
	dir, release, err := s.locked(scope)
	if err != nil {
		return WorkflowValidation{}, err
	}
	defer release()
	repo, err := s.repository(scope, dir)
	if err != nil {
		return WorkflowValidation{}, err
	}
	view, _, err := s.scoped(scope)
	if err != nil {
		return WorkflowValidation{}, err
	}
	return view.validateWorkflow(content, repo), nil
}
func validationError(v WorkflowValidation) error {
	if len(v.Diagnostics) == 0 {
		return errors.New("workflow validation failed")
	}
	d := v.Diagnostics[0]
	return fmt.Errorf("%s: %s: %s", d.Code, d.Field, d.Message)
}
func (s *Service) allows(c CommandCapability) bool {
	for _, allowed := range s.opts.CommandCapabilities {
		if c == allowed {
			return true
		}
	}
	return false
}
func (s *Service) validateWorkflow(content, repo string) WorkflowValidation {
	v := WorkflowValidation{Diagnostics: []WorkflowDiagnostic{}, Commands: []WorkflowCommand{}}
	fail := func(code, field string, err error) WorkflowValidation {
		v.Diagnostics = append(v.Diagnostics, WorkflowDiagnostic{code, "error", field, err.Error()})
		return v
	}
	if len(content) > 32768 {
		return fail("size", "workflow", errors.New("workflow exceeds 32 KiB"))
	}
	// sigs YAML-to-JSON decodes one document. Explicitly reject trailing
	// documents so review cannot contain an ignored second payload.
	decoder := yamlv3.NewDecoder(bytes.NewBufferString(content))
	var document any
	if err := decoder.Decode(&document); err != nil {
		return fail("yaml", "workflow", err)
	}
	if err := decoder.Decode(&document); !errors.Is(err, io.EOF) {
		return fail("yaml", "workflow", errors.New("exactly one YAML document is required"))
	}
	var raw map[string]any
	if err := yaml.UnmarshalStrict([]byte(content), &raw); err != nil {
		return fail("yaml", "workflow", err)
	}
	var wf apiv1.Workflow
	if err := yaml.UnmarshalStrict([]byte(content), &wf); err != nil {
		return fail("yaml", "workflow", err)
	}
	if err := onlyKeys(raw, "apiVersion", "kind", "dslVersion", "metadata", "spec"); err != nil {
		return fail("policy", "workflow", err)
	}
	if wf.Kind != "Workflow" || wf.APIVersion != "goobers.dev/v1alpha1" || (wf.DSLVersion != "2.0" && wf.DSLVersion != "3.0") || !namePattern.MatchString(wf.Name) || wf.Spec.Gaggle != s.policyGaggle() {
		return fail("policy", "metadata", errors.New("use DSL 2.0/3.0, a safe workflow name and the session-authorized gaggle"))
	}
	metadata, _ := raw["metadata"].(map[string]any)
	if err := onlyKeys(metadata, "name", "annotations"); err != nil {
		return fail("policy", "metadata", err)
	}
	for name, value := range wf.Annotations {
		if (name != "goobers.dev/acknowledge-manual-only" && (wf.DSLVersion != "3.0" || name != "goobers.dev/allow-preview-features")) || value != "true" {
			return fail("policy", "metadata.annotations", errors.New("only manual-only acknowledgment and DSL3 preview acknowledgment are authorized"))
		}
	}
	spec, _ := raw["spec"].(map[string]any)
	if err := onlyKeys(spec, "gaggle", "displayName", "enabled", "triggers", "readiness", "start", "tasks"); err != nil {
		return fail("policy", "spec", err)
	}
	triggers, _ := spec["triggers"].([]any)
	if len(triggers) != 1 || len(wf.Spec.Triggers) != 1 || wf.Spec.Triggers[0].Type != apiv1.TriggerManual {
		return fail("policy", "spec.triggers", errors.New("exactly one manual trigger is required"))
	}
	trigger, _ := triggers[0].(map[string]any)
	if err := onlyKeys(trigger, "type"); err != nil {
		return fail("policy", "spec.triggers", err)
	}
	if ready, ok := spec["readiness"]; ok {
		m, _ := ready.(map[string]any)
		if err := onlyKeys(m, "maxConcurrentRuns"); err != nil {
			return fail("policy", "spec.readiness", err)
		}
		if n, ok := m["maxConcurrentRuns"]; ok && n != float64(1) {
			return fail("policy", "spec.readiness", errors.New("maxConcurrentRuns must be 1"))
		}
	}
	if len(wf.Spec.Tasks) < 1 || len(wf.Spec.Tasks) > 8 {
		return fail("policy", "spec.tasks", errors.New("require 1–8 bounded deterministic stages"))
	}
	tasks, _ := spec["tasks"].([]any)
	bound := make([][]string, len(wf.Spec.Tasks))
	for i := range wf.Spec.Tasks {
		task := &wf.Spec.Tasks[i]
		field := fmt.Sprintf("spec.tasks[%d]", i)
		m, _ := tasks[i].(map[string]any)
		if err := onlyKeys(m, "name", "type", "goal", "timeoutSeconds", "run", "next", "runsOn"); err != nil {
			return fail("policy", field, err)
		}
		if task.Type != apiv1.TaskDeterministic || task.TimeoutSeconds < 1 || task.TimeoutSeconds > 30 || task.Run == nil {
			return fail("policy", field, errors.New("deterministic stages require 1–30 second limits and run controls"))
		}
		run, _ := m["run"].(map[string]any)
		if err := onlyKeys(run, "workspace", "network", "command"); err != nil {
			return fail("policy", field+".run", err)
		}
		if task.Run.Workspace != apiv1.WorkspaceScratch {
			return fail("policy", field+".run", errors.New("workspace:scratch is required"))
		}
		if wf.DSLVersion == "2.0" {
			if task.Run.Network != apiv1.NetworkNone || task.RunsOn != nil {
				return fail("policy", field+".run", errors.New("DSL2 requires run.network:none and no runsOn"))
			}
		} else {
			if task.Run.Network != "" || task.RunsOn == nil {
				return fail("policy", field+".runsOn", errors.New("DSL3 requires runsOn.restrictions:[network:none], without run.network"))
			}
			placement, _ := m["runsOn"].(map[string]any)
			if err := onlyKeys(placement, "restrictions"); err != nil {
				return fail("policy", field+".runsOn", err)
			}
			if len(task.RunsOn.Restrictions) != 1 || task.RunsOn.Restrictions[0] != "network:none" {
				return fail("policy", field+".runsOn", errors.New("only the enforced network:none restriction is authorized"))
			}
		}
		args := task.Run.Command
		if len(args) < 3 || args[0] != "node" {
			return fail("command", field+".run.command", errors.New("only node --test or node --check with explicit scoped files is authorized"))
		}
		var capability CommandCapability
		switch args[1] {
		case "--test":
			capability = CommandNodeTest
			if len(args) > 18 {
				return fail("command", field+".run.command", errors.New("node --test accepts 1–16 explicit .test.mjs files"))
			}
		case "--check":
			capability = CommandNodeCheck
			if len(args) != 3 {
				return fail("command", field+".run.command", errors.New("node --check accepts exactly one .mjs file"))
			}
		default:
			return fail("command", field+".run.command", errors.New("node flags are not authorized"))
		}
		if !s.allows(capability) {
			return fail("authority", field+".run.command", fmt.Errorf("host has not enabled %s", capability))
		}
		portable := append([]string{}, args[:2]...)
		resolved := append([]string{}, args[:2]...)
		seen := map[string]bool{}
		for _, arg := range args[2:] {
			rel, abs, err := scopedFile(repo, arg, capability)
			if err != nil {
				return fail("path", field+".run.command", err)
			}
			if seen[rel] {
				return fail("path", field+".run.command", errors.New("duplicate command target"))
			}
			seen[rel] = true
			portable = append(portable, rel)
			resolved = append(resolved, abs)
		}
		task.Run.Command = portable
		bound[i] = resolved
		v.Commands = append(v.Commands, WorkflowCommand{task.Name, capability, append([]string{}, portable...)})
	}
	// This subset is an acyclic chain. Reject hidden/unreachable extra work and
	// repeatable loops even if the full declarative DSL supports them.
	byName := map[string]string{}
	for _, task := range wf.Spec.Tasks {
		if _, exists := byName[task.Name]; exists {
			return fail("graph", "spec.tasks", errors.New("duplicate stage name"))
		}
		byName[task.Name] = task.Next
	}
	visited := map[string]bool{}
	for at := wf.Spec.Start; at != ""; {
		next, ok := byName[at]
		if !ok || visited[at] {
			return fail("graph", "spec.start", errors.New("stages must form an acyclic chain from start to an empty terminal next"))
		}
		visited[at] = true
		at = next
	}
	if len(visited) != len(wf.Spec.Tasks) {
		return fail("graph", "spec.tasks", errors.New("all stages must be reachable exactly once"))
	}
	validator, err := validate.New()
	if err != nil {
		return fail("native_schema", "workflow", err)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return fail("native_schema", "workflow", err)
	}
	if err = validator.ValidateJSON("workflow.schema.json", data); err != nil {
		return fail("native_schema", "workflow", err)
	}
	if _, err = workflow.Compile(workflow.Definition{Name: wf.Name, Version: 1, DSLVersion: wf.DSLVersion, Spec: wf.Spec, Annotations: wf.Annotations}, workflow.WithPreviewFeatures(workflow.PreviewFeaturesEnabled(wf.Annotations))); err != nil {
		return fail("native_compile", "workflow", err)
	}
	normalized, err := marshalConfig(wf)
	if err != nil {
		return fail("normalize", "workflow", err)
	}
	for i := range wf.Spec.Tasks {
		wf.Spec.Tasks[i].Run.Command = bound[i]
	}
	resolved, err := marshalConfig(wf)
	if err != nil {
		return fail("normalize", "workflow", err)
	}
	v.Valid = true
	v.Name = wf.Name
	v.NormalizedYAML = string(normalized)
	v.BoundYAML = string(resolved)
	v.PayloadDigest = digest(normalized)
	v.BindingDigest = digest([]byte(repo + "\x00" + string(resolved)))
	return v
}

func canonicalRepository(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("repository must be a directory")
	}
	return canonical, nil
}
func scopedFile(repo, arg string, capability CommandCapability) (string, string, error) {
	if arg == "" || strings.ContainsAny(arg, "\\\x00\r\n") {
		return "", "", errors.New("invalid scoped file path")
	}
	for _, part := range strings.Split(filepath.ToSlash(arg), "/") {
		if part == ".." || part == "." {
			return "", "", errors.New("path traversal is forbidden")
		}
	}
	if filepath.IsAbs(arg) {
		rel, err := filepath.Rel(repo, arg)
		if err != nil {
			return "", "", err
		}
		arg = rel
	}
	if strings.HasPrefix(arg, "-") || filepath.ToSlash(filepath.Clean(arg)) != arg {
		return "", "", errors.New("require a clean relative file path")
	}
	for _, part := range strings.Split(arg, "/") {
		if part == "" || part == "." || part == ".." || part == ".git" || part == "node_modules" {
			return "", "", errors.New("path traversal and metadata/dependency paths are forbidden")
		}
	}
	if !strings.HasSuffix(arg, ".mjs") || (capability == CommandNodeTest && !strings.HasSuffix(arg, ".test.mjs")) {
		return "", "", errors.New("command target has an unsupported extension")
	}
	at := repo
	for _, part := range strings.Split(arg, "/") {
		at = filepath.Join(at, part)
		info, err := os.Lstat(at)
		if err != nil {
			return "", "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", "", errors.New("symlink command targets are forbidden")
		}
	}
	info, err := os.Stat(at)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() {
		return "", "", errors.New("command target must be a regular file")
	}
	return arg, at, nil
}

// binding pins both repository identity and command authority across service
// restarts. Changing host wiring requires a fresh scope and fresh review.
func (s *Service) binding(scope Scope, dir string) (string, error) {
	repo, err := s.repository(scope, dir)
	if err != nil {
		return "", err
	}
	policy, err := s.policyFor(scope)
	if err != nil {
		return "", err
	}
	capabilities := append([]CommandCapability{}, policy.CommandCapabilities...)
	sort.Slice(capabilities, func(i, j int) bool { return capabilities[i] < capabilities[j] })
	source, err := s.configDir(scope)
	if err != nil {
		return "", err
	}
	data, _ := json.Marshal(struct {
		Repository   string
		Capabilities []CommandCapability
		Policy       Policy
		ConfigDir    string
	}{repo, capabilities, policy, source})
	path := filepath.Join(dir, "repository-binding.json")
	prior, err := os.ReadFile(path)
	if err == nil && string(prior) != string(data) {
		return "", errors.New("repository or command authority changed; open a fresh session scope")
	}
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err = atomicWrite(path, data); err != nil {
			return "", err
		}
	}
	return digest(data), nil
}
func (s *Service) defaultDraft(repo string, custom bool) (draft string) {
	defer func() {
		if s.policy != nil {
			draft = strings.Replace(draft, "gaggle: interactive", "gaggle: "+s.policy.Gaggle, 1)
		}
		if s.opts.PolicyForScope != nil {
			draft = strings.Replace(draft, `dslVersion: "2.0"`, `dslVersion: "3.0"`, 1)
			draft = strings.Replace(draft, "  annotations:", "  annotations:\n    goobers.dev/allow-preview-features: \"true\"", 1)
			draft = strings.ReplaceAll(draft, "      run:", "      runsOn: {restrictions: [network:none]}\n      run:")
			draft = strings.ReplaceAll(draft, "        network: none\n", "")
		}
	}()
	if s.allows(CommandNodeTest) {
		if _, _, err := scopedFile(repo, "invoice.test.mjs", CommandNodeTest); err == nil {
			name := "invoice-check"
			if custom {
				name += "-custom"
			}
			return workflowDraft(name, repo)
		}
	}
	var tests, checks []string
	_ = filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(repo, path)
		if strings.HasSuffix(rel, ".test.mjs") {
			tests = append(tests, rel)
		}
		if strings.HasSuffix(rel, ".mjs") {
			checks = append(checks, rel)
		}
		return nil
	})
	var target, flag string
	if s.allows(CommandNodeTest) && len(tests) > 0 {
		sort.Strings(tests)
		target, flag = tests[0], "--test"
	} else if s.allows(CommandNodeCheck) && len(checks) > 0 {
		sort.Strings(checks)
		target, flag = checks[0], "--check"
	} else {
		return ""
	}
	name := "repository-check"
	if custom {
		name += "-custom"
	}
	draft = workflowDraft(name, repo)
	old, _ := json.Marshal([]string{"node", "--test", filepath.Join(repo, "invoice.test.mjs")})
	command, _ := json.Marshal([]string{"node", flag, target})
	draft = strings.Replace(draft, string(old), string(command), 1)
	draft = strings.ReplaceAll(draft, "Invoice regression check", "Repository check")
	return strings.ReplaceAll(draft, "Run the actual invoice regression tests in this session workspace", "Check the scoped repository with the host-authorized Node capability")
}
