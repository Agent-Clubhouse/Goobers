package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

func TestConnectADOPreflightFailurePreservesFiles(t *testing.T) {
	t.Setenv("GOOBERS_ADO_TOKEN", "test-pat")
	stubConnectReachability(t, errors.New("repository access denied"))
	root := filepath.Join(t.TempDir(), "ado")
	code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	paths, err := filepath.Glob(filepath.Join(root, "config", "gaggles", "*", "gaggle.yaml"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("gaggles: %v %v", paths, err)
	}
	paths = append(paths, instance.NewLayout(root).ConfigFile())
	before := make(map[string]string)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = string(data)
	}
	code, _, stderr = runArgs(t, "connect", "contoso/boards/web", root)
	if code != 1 {
		t.Fatalf("connect: %d %s", code, stderr)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != before[path] {
			t.Fatalf("failed preflight changed %s: %v", path, err)
		}
	}
}

func TestConnectADORoundTrip(t *testing.T) {
	t.Setenv("GOOBERS_ADO_TOKEN", "")
	root := filepath.Join(t.TempDir(), "ado")
	code, _, stderr := runArgs(t, "init", "--template=standard", "--provider=ado", "--ci-command=[\"dotnet\",\"test\"]", "--required-capabilities=dotnet@8", root)
	if code != 0 {
		t.Fatalf("init: %d %s", code, stderr)
	}
	for attempt := range 2 {
		code, _, stderr = runArgs(t, "connect", "contoso/boards/web", root)
		if code != 0 {
			t.Fatalf("connect attempt %d: %d %s", attempt, code, stderr)
		}
		cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Repos) != 1 || cfg.Repos[0].Provider != "ado" || cfg.Repos[0].Owner != "contoso" || cfg.Repos[0].Project != "boards" || cfg.Repos[0].Name != "web" || cfg.Repos[0].Token.Env != "GOOBERS_ADO_TOKEN" {
			t.Fatalf("incorrect repository: %+v", cfg.Repos)
		}
		set, report, err := instance.LoadConfigDir(filepath.Join(root, "config"))
		if err != nil || report.HasErrors() {
			t.Fatalf("load: %v %+v", err, report)
		}
		if len(set.Gaggles) != 1 || set.Gaggles[0].Spec.Project.Owner != "contoso" || set.Gaggles[0].Spec.Project.Project != "boards" || set.Gaggles[0].Spec.Project.Name != "web" || set.Gaggles[0].Spec.Backlog.Project != "boards" {
			t.Fatalf("incorrect gaggle: %+v", set.Gaggles)
		}
	}
	before, err := os.ReadFile(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runArgs(t, "connect", "contoso/boards/web", root)
	after, err := os.ReadFile(instance.NewLayout(root).ConfigFile())
	if code != 0 || err != nil || string(before) != string(after) {
		t.Fatalf("repeat changed configuration: code=%d err=%v stderr=%s", code, err, stderr)
	}
}
