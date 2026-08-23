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
	return Definition{Name: w.Name, Version: 1, DSLVersion: w.DSLVersion, Spec: w.Spec}
}

// goldenDigests pins the compiled content digest of each shipped workflow. A
// change here means the compiled machine's shape changed — intended (bump the
// value) or a regression (investigate). Digests are over the definition's
// canonical form, so YAML reformatting alone never moves them.
var goldenDigests = map[string]string{
	// #2213/#2341: await-ci and PR-opening tasks declare provider-neutral PR
	// capabilities.
	// #2386: read-only duplicate discovery declares issue-read, not issue-write.
	// C+D2/#2861 wave: await-ci gained inputs.kind=ci-poll — its command was
	// always a placeholder (there is no `goobers ci-poll` CLI command), and the
	// new unknown-subcommand admission makes the kind dispatch explicit.
	"implementation.yaml":   "sha256:8a7ea8c6942f2a1bdbb94e56631573bf690c6e7dc4a18b23efbcc2eafc672df5",
	"backlog-curation.yaml": "sha256:73b4c30bc6a74f64d912709b7414a5e4ad52b08afbddc74dd3612df91b69cdbd",
	"work-nomination.yaml":  "sha256:e44ee1e09a463011056038605d75c65a930c3489a5e363d44b5e6ad8ad9fc627",
}

// TestShippedWorkflowsCompile proves the three V0 shipped workflows (curation,
// nomination, implementation) are expressible in schema v0 and compile to
// stable, digest-identical machines (issue #9 acceptance).
func TestShippedWorkflowsCompile(t *testing.T) {
	goobers := loadShippedGoobers(t)
	for _, file := range []string{"implementation.yaml", "backlog-curation.yaml", "work-nomination.yaml"} {
		t.Run(file, func(t *testing.T) {
			def := loadShippedWorkflow(t, file)

			m, err := compileAcknowledged(def, WithGoobers(goobers))
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			if m.Digest() == "" {
				t.Fatal("empty digest")
			}
			t.Logf("%s digest = %s", file, m.Digest())

			// Deterministic: recompiling the same definition yields the same digest.
			m2, err := compileAcknowledged(def, WithGoobers(goobers))
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
	m, err := compileAcknowledged(Definition{Name: w.Name, Version: 1, Spec: w.Spec})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	const want = "sha256:2f5c49dd24ca4361622268c979628ea1ce12a2d398210f72e1424dded501ff38"
	t.Logf("default-implement digest = %s", m.Digest())
	if m.Digest() != want {
		t.Errorf("digest drift for default-implement:\n got  %s\n want %s\n(update if intended)", m.Digest(), want)
	}
}

// TestDigestChangesWithContent guards that the digest actually commits to the
// definition: a semantic change must move it.
func TestDigestChangesWithContent(t *testing.T) {
	base := Definition{Name: "x", Version: 1, Spec: linearSpec()}
	m1, err := compileAcknowledged(base)
	if err != nil {
		t.Fatal(err)
	}
	changed := Definition{Name: "x", Version: 1, Spec: linearSpec()}
	changed.Spec.Tasks[0].Goal = "a different goal"
	m2, err := compileAcknowledged(changed)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Digest() == m2.Digest() {
		t.Error("expected digest to change when task goal changes")
	}
}

func TestDigestIncludesDSLVersionWhenPresent(t *testing.T) {
	unversioned := Definition{Name: "x", Version: 1, Spec: linearSpec()}
	m1, err := compileAcknowledged(unversioned)
	if err != nil {
		t.Fatal(err)
	}
	versioned := unversioned
	versioned.DSLVersion = "2.0"
	m2, err := compileAcknowledged(versioned)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Digest() == m2.Digest() {
		t.Fatal("expected dslVersion to be retained in the compiled definition digest")
	}
}
