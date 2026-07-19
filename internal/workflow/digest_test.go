package workflow

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

var docSep = regexp.MustCompile(`(?m)^---\s*$`)

func loadShippedGoobers(t *testing.T) map[string]apiv1.GooberSpec {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "shipped", "goobers.yaml"))
	if err != nil {
		t.Fatalf("read goobers: %v", err)
	}
	out := map[string]apiv1.GooberSpec{}
	for _, seg := range docSep.Split(string(raw), -1) {
		if strings.TrimSpace(seg) == "" {
			continue
		}
		var g apiv1.Goober
		if err := yaml.Unmarshal([]byte(seg), &g); err != nil {
			t.Fatalf("unmarshal goober: %v", err)
		}
		out[g.Name] = g.Spec
	}
	return out
}

func loadShippedWorkflow(t *testing.T, file string) Definition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "shipped", file))
	if err != nil {
		t.Fatalf("read %s: %v", file, err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal %s: %v", file, err)
	}
	return Definition{Name: w.Name, Version: 1, Spec: w.Spec}
}

// goldenDigests pins the compiled content digest of each shipped workflow. A
// change here means the compiled machine's shape changed — intended (bump the
// value) or a regression (investigate). Digests are over the definition's
// canonical form, so YAML reformatting alone never moves them.
var goldenDigests = map[string]string{
	// #401: await-ci explicitly declares the github:pr:write capability used
	// to poll the pull request's checks.
	"implementation.yaml":   "sha256:caf91733d1e9aa26529694ddcbe28c4ed64c99f70c87f3b9e5d863df747722b4",
	"backlog-curation.yaml": "sha256:99bf677d0976f720638d16fe95cf88802da632011bf5e7e1d2ca0c0dabf99bd8",
	"work-nomination.yaml":  "sha256:0e689d382fa5e62493e7f339a9caefc28e4e2ce3a63054035e7955b84de2c10c",
}

// TestShippedWorkflowsCompile proves the three V0 shipped workflows (curation,
// nomination, implementation) are expressible in schema v0 and compile to
// stable, digest-identical machines (issue #9 acceptance).
func TestShippedWorkflowsCompile(t *testing.T) {
	goobers := loadShippedGoobers(t)
	for _, file := range []string{"implementation.yaml", "backlog-curation.yaml", "work-nomination.yaml"} {
		t.Run(file, func(t *testing.T) {
			def := loadShippedWorkflow(t, file)

			m, err := Compile(def, WithGoobers(goobers))
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if m.Digest() == "" {
				t.Fatal("empty digest")
			}
			t.Logf("%s digest = %s", file, m.Digest())

			// Deterministic: recompiling the same definition yields the same digest.
			m2, err := Compile(def, WithGoobers(goobers))
			if err != nil {
				t.Fatalf("recompile: %v", err)
			}
			if m.Digest() != m2.Digest() {
				t.Errorf("digest not stable across compiles: %s vs %s", m.Digest(), m2.Digest())
			}
			if want := goldenDigests[file]; m.Digest() != want {
				t.Errorf("digest drift for %s:\n got  %s\n want %s\n(update goldenDigests if the change is intended)", file, m.Digest(), want)
			}
		})
	}
}

// TestExampleConfigWorkflowCompiles compiles the reference definition shipped in
// config-examples/ and pins its digest — the config-examples golden per issue #9.
func TestExampleConfigWorkflowCompiles(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config-examples", "gaggles", "acme-web", "workflows", "default-implement.yaml"))
	if err != nil {
		t.Fatalf("read example workflow: %v", err)
	}
	var w apiv1.Workflow
	if err := yaml.Unmarshal(raw, &w); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	m, err := Compile(Definition{Name: w.Name, Version: 1, Spec: w.Spec})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	const want = "sha256:b9dbc3c025036d2b8030481339a094fd82108cf3d9835d84d545008c2293ff2c"
	t.Logf("default-implement digest = %s", m.Digest())
	if m.Digest() != want {
		t.Errorf("digest drift for default-implement:\n got  %s\n want %s\n(update if intended)", m.Digest(), want)
	}
}

// TestDigestChangesWithContent guards that the digest actually commits to the
// definition: a semantic change must move it.
func TestDigestChangesWithContent(t *testing.T) {
	base := Definition{Name: "x", Version: 1, Spec: linearSpec()}
	m1, err := Compile(base)
	if err != nil {
		t.Fatal(err)
	}
	changed := Definition{Name: "x", Version: 1, Spec: linearSpec()}
	changed.Spec.Tasks[0].Goal = "a different goal"
	m2, err := Compile(changed)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Digest() == m2.Digest() {
		t.Error("expected digest to change when task goal changes")
	}
}
