package providermatrix

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// topologyBBacklog is the neutral GitHub backlog repository the topology (b)
// column moves every gaggle's backlog onto.
const topologyBBacklog = "example-org/example-backlog"

// TestShippedWorkflowsCompileOnTopologyB is the compile-matrix gate's
// topology (b) column (docs/design/ado-parity-dsl-2-0.md §7.2 step 6, §8.1;
// ADO-N31): every shipped workflow tree and scaffold, with its code on Azure
// DevOps and its backlog on GitHub, passes full validation and CONF-6. There
// is no expected-failure list for this column: a (b) gaggle validates exactly
// when the same subject validates on ADO.
func TestShippedWorkflowsCompileOnTopologyB(t *testing.T) {
	t.Parallel()
	for _, subject := range matrixSubjects(t) {
		dir := subject.build(t, apiv1.ProviderADO)
		moveBacklogsToGitHub(t, dir)
		findings, workflows := compileSubject(t, subject.name+"/topology-b", apiv1.ProviderADO, dir)
		if workflows == 0 && len(findings) == 0 {
			t.Fatalf("%s in topology (b) loaded no workflows; the gate would pass vacuously", subject.name)
		}
		t.Logf("%s in topology (b): %d workflow(s), %d finding(s)", subject.name, workflows, len(findings))
		for _, f := range findings {
			t.Errorf("topology (b) compile-matrix failure: %s", f)
		}
	}
}

// TestTopologyBColumnIsReallyMixed proves the column exercises what it
// claims: every rewritten gaggle keeps its project on ADO and its backlog on
// GitHub.
func TestTopologyBColumnIsReallyMixed(t *testing.T) {
	t.Parallel()
	subjects := matrixSubjects(t)
	dir := subjects[0].build(t, apiv1.ProviderADO)
	moveBacklogsToGitHub(t, dir)
	set, _, err := instance.LoadConfigDir(dir)
	if err != nil {
		t.Fatalf("load %s in topology (b): %v", subjects[0].name, err)
	}
	if len(set.Gaggles) == 0 {
		t.Fatal("no gaggles loaded")
	}
	for _, g := range set.Gaggles {
		if g.Spec.Project.Provider != apiv1.ProviderADO || g.Spec.Backlog.Provider != apiv1.ProviderGitHub || g.Spec.Backlog.Project != topologyBBacklog {
			t.Errorf("gaggle %s: project %s, backlog %s %q; want ADO code with the GitHub backlog %s",
				g.Name, g.Spec.Project.Provider, g.Spec.Backlog.Provider, g.Spec.Backlog.Project, topologyBBacklog)
		}
	}
}

// moveBacklogsToGitHub rewrites every gaggle.yaml under dir so its backlog
// lives in the GitHub repository topologyBBacklog, leaving its project as is.
func moveBacklogsToGitHub(t *testing.T, dir string) {
	t.Helper()
	rewritten := 0
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || filepath.Base(path) != "gaggle.yaml" {
			return walkErr
		}
		moveBacklogToGitHub(t, path)
		rewritten++
		return nil
	})
	if err != nil {
		t.Fatalf("rewrite backlogs under %s: %v", dir, err)
	}
	if rewritten == 0 {
		t.Fatalf("no gaggle.yaml under %s; the topology (b) column would be vacuous", dir)
	}
}

func moveBacklogToGitHub(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	spec, _ := doc["spec"].(map[string]any)
	backlog, _ := spec["backlog"].(map[string]any)
	if backlog == nil {
		t.Fatalf("%s has no spec.backlog", path)
	}
	backlog["provider"] = string(apiv1.ProviderGitHub)
	backlog["project"] = topologyBBacklog
	delete(backlog, "baseUrl")
	delete(backlog, "doneStates")
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
