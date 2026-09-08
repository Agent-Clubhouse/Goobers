package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestInitStandardNonInteractive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "standard")
	code, stdout, stderr := runArgs(t, "init", "--template=standard", "--harness=claude-code", "--ci-command=[\"npm\",\"run\",\"ci\"]", "--required-capabilities=node@24", root)
	if code != 0 {
		t.Fatalf("init code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	set, report, err := instance.LoadConfigDir(filepath.Join(root, "config"))
	if err != nil {
		t.Fatalf("LoadConfigDir: %v (report: %+v)", err, report)
	}
	if len(set.Workflows) != 2 || len(set.Goobers) != 3 {
		t.Fatalf("got %d workflows and %d goobers, want canonical pair and its three personas", len(set.Workflows), len(set.Goobers))
	}
	for _, wf := range set.Workflows {
		if wf.Name != "implementation" && wf.Name != "backlog-curation" {
			t.Errorf("unexpected workflow %q", wf.Name)
		}
		for _, task := range wf.Spec.Tasks {
			if task.Name == "local-ci" && !reflect.DeepEqual(task.Run.Command, []string{"npm", "run", "ci"}) {
				t.Errorf("local CI command=%v", task.Run.Command)
			}
		}
	}
	for _, goober := range set.Goobers {
		if goober.Spec.Harness != "claude-code" {
			t.Errorf("%s harness=%s", goober.Name, goober.Spec.Harness)
		}
	}
	if !reflect.DeepEqual(set.Gaggles[0].Spec.CICommand, []string{"npm", "run", "ci"}) {
		t.Fatalf("gaggle CI command=%v", set.Gaggles[0].Spec.CICommand)
	}
}

func TestInitStandardADO(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ado")
	code, stdout, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("ADO init code=%d stdout=%s stderr=%s", code, stdout, stderr)
	}
	set, report, err := instance.LoadConfigDir(filepath.Join(root, "config"))
	if err != nil || report.HasErrors() {
		t.Fatalf("load: %v report=%+v", err, report)
	}
	if len(set.Gaggles) != 1 || set.Gaggles[0].Spec.Project.Provider != "ado" || set.Gaggles[0].Spec.Project.Project != "your-project" || set.Gaggles[0].Spec.Backlog.Provider != "ado" || set.Gaggles[0].Spec.Backlog.Project != "your-project" {
		t.Fatalf("ADO identity not preserved: %+v", set.Gaggles)
	}
	data, err := os.ReadFile(filepath.Join(root, "instance.yaml"))
	if err != nil || !strings.Contains(string(data), "GOOBERS_ADO_TOKEN") || strings.Contains(string(data), "GOOBERS_GITHUB") {
		t.Fatalf("ADO credentials incorrect: %s %v", data, err)
	}
}

func TestInitStandardRejectsUnsafeOrIncompleteOptions(t *testing.T) {
	for _, extra := range [][]string{
		{}, {"--ci-command=[]", "--required-capabilities=node@24"},
		{"--ci-command=[\"npm\",\"\"]", "--required-capabilities=node@24"},
		{"--ci-command=[\"npm\"]"},
		{"--ci-command=[\"npm\"]", "--required-capabilities=invalid capability"},
		{"--ci-command=[\"npm\"] trailing", "--required-capabilities=node@24"},
	} {
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "absent")
			args := append([]string{"init", "--template=standard"}, extra...)
			code, _, stderr := runArgs(t, append(args, root)...)
			if code == 0 {
				t.Fatalf("invalid options accepted: %v", extra)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("invalid init wrote destination: %v; %s", err, stderr)
			}
		})
	}
}

func TestInitStandardPreservesExistingTarget(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "instance.yaml")
	if err := os.WriteFile(path, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, _ := runArgs(t, "init", "--template=standard", "--ci-command=[\"npm\"]", "--required-capabilities=node@24", root)
	if code == 0 {
		t.Fatal("populated destination accepted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep me" {
		t.Fatalf("existing file changed: %q %v", data, err)
	}
}

func TestInitStandardModeBoundaries(t *testing.T) {
	for _, flags := range [][]string{
		{"--provider=ado"},
		{"--template=quickstart", "--provider=ado"},
		{"--template=standard", "--ci-command=[\"npm\"]", "--required-capabilities=node@24", "--provider=unknown"},
		{"--ci-command=[\"npm\"]", "--required-capabilities=node@24"},
		{"--template=quickstart", "--ci-command=[\"npm\"]"},
		{"--template=standard", "--ci-command=[\"npm\"]", "--required-capabilities=node@24", "--demo"},
		{"--template=standard", "--ci-command=[\"npm\"]", "--required-capabilities=node@24", "--guided"},
		{"--template=standard", "--ci-command=[\"npm\"]", "--required-capabilities=node@24", "--harness=unknown"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "absent")
			args := append([]string{"init"}, flags...)
			code, _, stderr := runArgs(t, append(args, root)...)
			if code != 2 {
				t.Fatalf("usage code=%d: %s", code, stderr)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatalf("invalid mode created target: %v", err)
			}
		})
	}
}
