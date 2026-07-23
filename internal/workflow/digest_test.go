package workflow

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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
	// #401: await-ci explicitly declares the github:pr:write capability used
	// to poll the pull request's checks.
	"implementation.yaml":   "sha256:3c0fb6f9133f0df14b208acfdaf6d96173dfd8a24771ce521035862ece080761",
	"backlog-curation.yaml": "sha256:268e996fa834f22c854680b2084cdeb054a437bed5d3546ea0755e19c86af151",
	"work-nomination.yaml":  "sha256:68692fb377f3140fa66033eec2fe00bfb0033b08c39783b370223622180a81e9",
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
	const want = "sha256:8799d6c3e5b977b82c6451462f025e60bc177307409dc70e00c4293040b01ddb"
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
	versioned.DSLVersion = "1.4"
	m2, err := compileAcknowledged(versioned)
	if err != nil {
		t.Fatal(err)
	}
	if m1.Digest() == m2.Digest() {
		t.Fatal("expected dslVersion to be retained in the compiled definition digest")
	}
}

func TestGooberDigestTracksEffectiveParticipatingGoobers(t *testing.T) {
	def := Definition{Name: "x", Version: 1, Spec: linearSpec()}
	def.Spec.Tasks[0].Type = apiv1.TaskAgentic
	def.Spec.Tasks[0].Goober = "coder"
	def.Spec.Tasks[0].Run = nil

	base := apiv1.GooberSpec{
		Instructions: "instructions.md",
		Skills:       []string{"testing", "go"},
		Model:        "model-a",
		Harness:      apiv1.HarnessCopilot,
		HarnessOptions: map[string]apiextensionsv1.JSON{
			"reasoningEffort": {Raw: []byte(`"high"`)},
		},
	}
	compile := func(spec apiv1.GooberSpec, instructions string) *Machine {
		t.Helper()
		machine, err := compileAcknowledged(
			def,
			WithGoobers(map[string]apiv1.GooberSpec{
				"coder": spec,
				"unused": {
					Instructions: "unused.md",
					Model:        "unrelated",
				},
			}),
			WithGooberInstructions(map[string]string{
				"coder":  instructions,
				"unused": "unused instructions",
			}),
		)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if machine.GooberDigest() == "" {
			t.Fatal("empty goober digest")
		}
		return machine
	}

	original := compile(base, "original instructions")
	for _, tc := range []struct {
		name         string
		spec         apiv1.GooberSpec
		instructions string
	}{
		{name: "instructions content", spec: base, instructions: "changed instructions"},
		{name: "skills", spec: func() apiv1.GooberSpec {
			spec := base
			spec.Skills = []string{"testing", "go", "security"}
			return spec
		}(), instructions: "original instructions"},
		{name: "model", spec: func() apiv1.GooberSpec {
			spec := base
			spec.Model = "model-b"
			return spec
		}(), instructions: "original instructions"},
		{name: "harness", spec: func() apiv1.GooberSpec {
			spec := base
			spec.Harness = apiv1.Harness("alternate")
			return spec
		}(), instructions: "original instructions"},
		{name: "harness options", spec: func() apiv1.GooberSpec {
			spec := base
			spec.HarnessOptions = map[string]apiextensionsv1.JSON{
				"reasoningEffort": {Raw: []byte(`"low"`)},
			}
			return spec
		}(), instructions: "original instructions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := compile(tc.spec, tc.instructions)
			if changed.GooberDigest() == original.GooberDigest() {
				t.Fatalf("goober digest did not change: %s", changed.GooberDigest())
			}
			if changed.Digest() != original.Digest() {
				t.Fatalf("workflow digest changed with goober identity: %s != %s", changed.Digest(), original.Digest())
			}
		})
	}

	reordered := base
	reordered.Instructions = "renamed.md"
	reordered.Skills = []string{"go", "testing", "go"}
	equivalent := compile(reordered, "original instructions")
	if equivalent.GooberDigest() != original.GooberDigest() {
		t.Fatalf("path or set ordering changed goober digest: %s != %s", equivalent.GooberDigest(), original.GooberDigest())
	}
}
