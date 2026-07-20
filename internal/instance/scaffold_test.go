package instance

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestInitFresh(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")

	res, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("expected nothing skipped on a fresh init, got %v", res.Skipped)
	}
	wantCreated := []string{ConfigFileName, ConfigDirName, RunsDirName, SchedulerDirName, WorkcopiesDirName, TelemetryDBName}
	gotCreated := append([]string(nil), res.Created...)
	sort.Strings(gotCreated)
	sort.Strings(wantCreated)
	if len(gotCreated) != len(wantCreated) {
		t.Fatalf("created = %v, want %v", res.Created, wantCreated)
	}
	for i := range wantCreated {
		if gotCreated[i] != wantCreated[i] {
			t.Fatalf("created = %v, want %v", res.Created, wantCreated)
		}
	}

	l := NewLayout(root)
	for _, dir := range []string{l.RunsDir(), l.SchedulerDir(), l.WorkcopiesDir(), l.ConfigDir()} {
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("expected %s to be a directory: %v", dir, err)
		}
	}
	if info, err := os.Stat(l.TelemetryDB()); err != nil || info.IsDir() {
		t.Fatalf("expected %s to be a file: %v", l.TelemetryDB(), err)
	}

	cfg, err := LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatalf("scaffolded instance.yaml did not load: %v", err)
	}
	if len(cfg.Repos) == 0 {
		t.Fatalf("expected scaffolded instance.yaml to include a starter repo entry")
	}

	if _, err := os.Stat(filepath.Join(l.ConfigDir(), "manifest.yaml")); err != nil {
		t.Fatalf("expected seeded config/manifest.yaml: %v", err)
	}

	set, report, err := LoadConfigDir(l.ConfigDir())
	if err != nil {
		t.Fatalf("seeded config/ did not validate: %v (report: %+v)", err, report)
	}
	if len(set.Gaggles) != 1 || len(set.Goobers) != 1 || len(set.Workflows) != 1 {
		t.Fatalf("unexpected seeded config shape: %+v", set)
	}
	assertDefaultImplementationPublishesPR(t, set.Workflows[0])
}

func assertDefaultImplementationPublishesPR(t *testing.T, workflow apiv1.Workflow) {
	t.Helper()
	if workflow.Name != "default-implement" || workflow.Spec.Start != "query-backlog" {
		t.Fatalf("unexpected default implementation entrypoint: %+v", workflow)
	}
	if len(workflow.Spec.Tasks) != 5 {
		t.Fatalf("default implementation tasks = %d, want 5", len(workflow.Spec.Tasks))
	}
	tasks := make(map[string]apiv1.Task, len(workflow.Spec.Tasks))
	for _, task := range workflow.Spec.Tasks {
		tasks[task.Name] = task
	}
	if task := tasks["query-backlog"]; task.Next != "implement" {
		t.Fatalf("query-backlog next = %q, want implement", task.Next)
	}
	if task := tasks["implement"]; task.Type != apiv1.TaskAgentic ||
		len(task.Capabilities) != 1 || task.Capabilities[0] != "repo:push" ||
		task.Next != "push-branch" {
		t.Fatalf("implement task does not hand committed work to push-branch: %+v", task)
	}
	if task := tasks["push-branch"]; task.Type != apiv1.TaskDeterministic ||
		task.Run == nil || len(task.Run.Command) != 2 ||
		task.Run.Command[0] != "goobers" || task.Run.Command[1] != "push-branch" ||
		task.Next != "open-pr" {
		t.Fatalf("push-branch task does not publish before PR creation: %+v", task)
	}
	if task := tasks["open-pr"]; task.Type != apiv1.TaskDeterministic ||
		task.Run == nil || len(task.Run.Command) != 2 ||
		task.Run.Command[0] != "goobers" || task.Run.Command[1] != "open-pr" ||
		task.Inputs["resultFile"] != "pr-result.json" ||
		len(task.ExpectedOutputs) != 2 ||
		task.ExpectedOutputs[0] != "pull-request-url" ||
		task.ExpectedOutputs[1] != "prNumber" ||
		task.Next != "close-out" {
		t.Fatalf("open-pr task does not verify PR creation before close-out: %+v", task)
	}
	if task := tasks["close-out"]; task.Type != apiv1.TaskDeterministic ||
		task.Run == nil || len(task.Run.Command) != 2 ||
		task.Run.Command[0] != "goobers" || task.Run.Command[1] != "issue-close-out" ||
		task.Inputs["status"] != "in-review" {
		t.Fatalf("close-out task does not keep the issue open for merge: %+v", task)
	}
}

func TestInitDemoFresh(t *testing.T) {
	root := filepath.Join(t.TempDir(), "demo")
	res, err := InitDemo(root)
	if err != nil {
		t.Fatalf("InitDemo: %v", err)
	}
	if len(res.Skipped) != 0 {
		t.Fatalf("fresh demo init skipped entries: %v", res.Skipped)
	}

	l := NewLayout(root)
	cfg, err := LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Repos) != 0 || len(cfg.Credentials) != 0 {
		t.Fatalf("demo instance unexpectedly requires connections: %+v", cfg)
	}
	set, report, err := LoadConfigDir(l.ConfigDir())
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	if len(set.Gaggles) != 1 || len(set.Goobers) != 0 || len(set.Workflows) != 1 {
		t.Fatalf("unexpected demo config shape: %+v", set)
	}
	workflow := set.Workflows[0]
	if workflow.Name != "demo" || len(workflow.Spec.Tasks) != 2 || len(workflow.Spec.Gates) != 1 {
		t.Fatalf("unexpected demo workflow: %+v", workflow)
	}
	for _, task := range workflow.Spec.Tasks {
		if task.Type != apiv1.TaskDeterministic || task.Run == nil ||
			task.Run.Workspace != apiv1.WorkspaceScratch || task.Run.Network != apiv1.NetworkNone {
			t.Fatalf("demo task is not an offline scratch deterministic stage: %+v", task)
		}
	}
}

func TestInitIdempotent(t *testing.T) {
	root := t.TempDir()

	if _, err := Init(root); err != nil {
		t.Fatalf("first Init: %v", err)
	}
	res, err := Init(root)
	if err != nil {
		t.Fatalf("second Init: %v", err)
	}
	if len(res.Created) != 0 {
		t.Fatalf("expected nothing created on a repeated init, got %v", res.Created)
	}
	if len(res.Skipped) != 6 {
		t.Fatalf("expected every piece skipped on a repeated init, got %v", res.Skipped)
	}
}

func TestInitPreservesExistingConfigDir(t *testing.T) {
	root := t.TempDir()
	l := NewLayout(root)
	if err := os.MkdirAll(l.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(l.ConfigDir(), "custom.yaml")
	if err := os.WriteFile(custom, []byte("kind: Manifest\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	found := false
	for _, s := range res.Skipped {
		if s == ConfigDirName {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected config dir to be skipped (pre-existing content), got created=%v skipped=%v", res.Created, res.Skipped)
	}
	if _, err := os.Stat(filepath.Join(l.ConfigDir(), "manifest.yaml")); err == nil {
		t.Fatal("starter config should not have been seeded over an existing config/ dir")
	}
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("pre-existing config file was not preserved: %v", err)
	}
}

func TestInitPreservesExistingInstanceYAML(t *testing.T) {
	root := t.TempDir()
	l := NewLayout(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	custom := `apiVersion: goobers.dev/v1alpha1
kind: Instance
repos:
  - provider: github
    owner: mine
    name: repo
    token:
      env: MY_TOKEN
`
	if err := os.WriteFile(l.ConfigFile(), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Init(root)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	found := false
	for _, s := range res.Skipped {
		if s == ConfigFileName {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected instance.yaml to be skipped, got created=%v skipped=%v", res.Created, res.Skipped)
	}
	cfg, err := LoadConfig(l.ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0].Owner != "mine" {
		t.Fatalf("pre-existing instance.yaml was overwritten: %+v", cfg.Repos)
	}
}
