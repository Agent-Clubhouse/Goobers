package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	goobersassets "github.com/goobers/goobers"
	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/agentkit"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/supportmatrix"
)

func TestAgentToolkitGoldenLayout(t *testing.T) {
	assets, err := collectAgentToolkitAssets(agentToolkitRepoRoot(t), agentToolkitRelease{
		SchemaVersion: agentToolkitSchemaVersion,
		BundleVersion: agentToolkitBundleVersion,
		Producer:      agentToolkitProducer{Version: "v1.2.3", Commit: "abc123"},
		DSLVersions:   supportmatrix.GetDSL().Versions(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for _, asset := range assets {
		fmt.Fprintln(&got, asset.Path)
	}
	want, err := os.ReadFile("testdata/agent-toolkit-layout.golden")
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != string(want) {
		t.Errorf("agent toolkit layout drifted (-want +got):\nwant:\n%s\ngot:\n%s", want, got.String())
	}
}

func TestAgentToolkitArchiveContract(t *testing.T) {
	repoRoot := agentToolkitRepoRoot(t)
	archivePath, err := packageAgentToolkit(repoRoot, "v9.8.7", "deadbeef1234", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filepath.Base(archivePath), "goobers-agent-toolkit_v9.8.7.zip"; got != want {
		t.Fatalf("archive name = %q, want %q", got, want)
	}

	entries := readAgentToolkitArchive(t, archivePath)
	manifestJSON, ok := entries["manifest.json"]
	if !ok {
		t.Fatal("archive is missing manifest.json")
	}
	validator, err := validate.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.ValidateJSON(schemas.AgentToolkitManifest, manifestJSON); err != nil {
		t.Fatalf("manifest does not match schema: %v", err)
	}

	var manifest agentToolkitManifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		t.Fatal(err)
	}
	embedded, err := agentkit.Build(goobersassets.AgentToolkitAssets, "v9.8.7", "deadbeef1234")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestJSON, embedded.ManifestJSON) {
		t.Fatal("release artifact manifest differs from the binary-embedded toolkit manifest")
	}
	for installedPath, file := range embedded.Files {
		transportPath := "payload/" + installedPath
		if !bytes.Equal(entries[transportPath], file.Data) {
			t.Errorf("release artifact %q differs from the binary-embedded toolkit asset", transportPath)
		}
	}
	if manifest.Producer != (agentToolkitProducer{Version: "v9.8.7", Commit: "deadbeef1234"}) {
		t.Errorf("producer = %+v", manifest.Producer)
	}
	if !reflect.DeepEqual(manifest.DSLVersions, supportmatrix.GetDSL().Versions()) {
		t.Errorf("DSL versions = %+v, want compiled support matrix", manifest.DSLVersions)
	}
	if manifest.Ownership.ProductRoot != agentToolkitProductRoot {
		t.Errorf("product root = %q", manifest.Ownership.ProductRoot)
	}
	for _, userPath := range []string{"AGENTS.md", "CLAUDE.md", ".github/copilot-instructions.md"} {
		if _, exists := entries[userPath]; exists {
			t.Errorf("user-owned instruction %q must not be a payload asset", userPath)
		}
	}

	inventory := make(map[string]agentToolkitAsset, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		if _, duplicate := inventory[asset.Path]; duplicate {
			t.Fatalf("manifest contains duplicate asset %q", asset.Path)
		}
		inventory[asset.Path] = asset
		data, exists := entries[asset.Path]
		if !exists {
			t.Errorf("manifest asset %q is missing from archive", asset.Path)
			continue
		}
		sum := sha256.Sum256(data)
		if got := fmt.Sprintf("%x", sum); got != asset.SHA256 {
			t.Errorf("%s digest = %s, want %s", asset.Path, got, asset.SHA256)
		}
		if got := int64(len(data)); got != asset.Size {
			t.Errorf("%s size = %d, want %d", asset.Path, got, asset.Size)
		}
	}
	for path := range entries {
		if path == "manifest.json" {
			continue
		}
		if _, exists := inventory[path]; !exists {
			t.Errorf("product payload file %q is not identified by the manifest", path)
		}
	}

	var release agentToolkitRelease
	if err := json.Unmarshal(entries[agentToolkitProductRoot+"/release.json"], &release); err != nil {
		t.Fatal(err)
	}
	if release.Producer != manifest.Producer || !reflect.DeepEqual(release.DSLVersions, manifest.DSLVersions) {
		t.Errorf("installed release metadata does not match manifest: %+v", release)
	}

	assertAgentToolkitReferencesMatchRelease(t, repoRoot, entries)
	assertAgentToolkitCapabilitiesMatchRelease(t, entries)
	assertAgentToolkitSchemaTermsMatchRelease(t, repoRoot, entries)
	assertAgentToolkitRelativeLinksResolve(t, entries)
	assertAgentToolkitAdaptersResolve(t, manifest, entries)
	assertAgentToolkitSafeContent(t, repoRoot, entries)
}

func TestAgentToolkitRejectsUnsafeProducerMetadata(t *testing.T) {
	if _, err := packageAgentToolkit(
		agentToolkitRepoRoot(t),
		"/Users/example/build",
		"deadbeef",
		t.TempDir(),
	); err == nil || !strings.Contains(err.Error(), "validate agent toolkit manifest") {
		t.Fatalf("unsafe producer error = %v", err)
	}
}

func assertAgentToolkitReferencesMatchRelease(t *testing.T, repoRoot string, entries map[string][]byte) {
	t.Helper()
	references := map[string]string{
		"skills/goobers-getting-started/SKILL.md":               "skills/goobers-getting-started/SKILL.md",
		"skills/goobers-dsl-author/SKILL.md":                    "skills/goobers-dsl-author/SKILL.md",
		"docs/ARCHITECTURE.md":                                  "docs/ARCHITECTURE.md",
		"api/schemas/workflow.schema.json":                      "api/schemas/workflow.schema.json",
		"config-examples/manifest.yaml":                         "config-examples/manifest.yaml",
		"internal/capability/capability.go":                     "internal/capability/capability.go",
		"docs/requirements/workflow.md":                         "docs/requirements/workflow.md",
		"skills/goobers-dsl-author/references/dsl-reference.md": "skills/goobers-dsl-author/references/dsl-reference.md",
	}
	for source, bundled := range references {
		want, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(source)))
		if err != nil {
			t.Fatal(err)
		}
		got, exists := entries[agentToolkitProductRoot+"/"+bundled]
		if !exists {
			t.Errorf("release reference %q is not bundled", bundled)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("bundled reference %q does not match release source %q", bundled, source)
		}
	}
}

func assertAgentToolkitCapabilitiesMatchRelease(t *testing.T, entries map[string][]byte) {
	t.Helper()
	reference := entries[agentToolkitProductRoot+"/skills/goobers-dsl-author/references/dsl-reference.md"]
	start := bytes.Index(reference, []byte("## Canonical capabilities"))
	if start < 0 {
		t.Fatal("bundled DSL reference has no canonical capabilities section")
	}
	reference = reference[start:]
	if end := bytes.Index(reference[len("## Canonical capabilities"):], []byte("\n## ")); end >= 0 {
		reference = reference[:len("## Canonical capabilities")+end]
	}
	rowPattern := regexp.MustCompile("(?m)^\\| `([^`]+)` \\|")
	documented := make([]string, 0)
	for _, match := range rowPattern.FindAllSubmatch(reference, -1) {
		documented = append(documented, string(match[1]))
	}
	sort.Strings(documented)
	want := make([]string, 0)
	for _, item := range capability.All() {
		name := string(item)
		if !capability.StageDeclarable(name) {
			continue
		}
		want = append(want, name)
	}
	sort.Strings(want)
	if !reflect.DeepEqual(documented, want) {
		t.Errorf("bundled DSL reference capabilities = %v, want exact canonical set %v", documented, want)
	}
}

func assertAgentToolkitSchemaTermsMatchRelease(t *testing.T, repoRoot string, entries map[string][]byte) {
	t.Helper()
	reference := entries[agentToolkitProductRoot+"/skills/goobers-dsl-author/references/dsl-reference.md"]
	for check := range gate.DefaultChecks() {
		if !bytes.Contains(reference, []byte("`"+check+"`")) {
			t.Errorf("bundled DSL reference is missing automated evaluator %q", check)
		}
	}
	for _, harness := range harnessConstants(t, repoRoot) {
		if !bytes.Contains(reference, []byte("`"+harness+"`")) {
			t.Errorf("bundled DSL reference is missing harness %q", harness)
		}
	}
}

func harnessConstants(t *testing.T, repoRoot string) []string {
	t.Helper()
	filename := filepath.Join(repoRoot, "api", "v1alpha1", "goober_types.go")
	file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var harnesses []string
	for _, declaration := range file.Decls {
		general, ok := declaration.(*ast.GenDecl)
		if !ok || general.Tok != token.CONST {
			continue
		}
		for _, spec := range general.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Values) != 1 {
				continue
			}
			identifier, ok := value.Type.(*ast.Ident)
			literal, literalOK := value.Values[0].(*ast.BasicLit)
			if !ok || identifier.Name != "Harness" || !literalOK || literal.Kind != token.STRING {
				continue
			}
			harness, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			harnesses = append(harnesses, harness)
		}
	}
	if len(harnesses) == 0 {
		t.Fatal("api/v1alpha1 declares no Harness constants")
	}
	return harnesses
}

func assertAgentToolkitRelativeLinksResolve(t *testing.T, entries map[string][]byte) {
	t.Helper()
	linkPattern := regexp.MustCompile(`\[[^]]*\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	schemePattern := regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*:`)
	for source, body := range entries {
		if !strings.HasSuffix(strings.ToLower(source), ".md") {
			continue
		}
		for _, match := range linkPattern.FindAllSubmatch(body, -1) {
			target := string(match[1])
			if strings.HasPrefix(target, "#") || strings.HasPrefix(target, "/") ||
				schemePattern.MatchString(target) {
				continue
			}
			target, _, _ = strings.Cut(target, "#")
			target, _, _ = strings.Cut(target, "?")
			resolved := path.Clean(path.Join(path.Dir(source), target))
			if resolved == "." || hasArchivePath(entries, resolved) {
				continue
			}
			t.Errorf("%s contains relative link %q to an unbundled target", source, string(match[1]))
		}
	}
}

func hasArchivePath(entries map[string][]byte, target string) bool {
	if _, exists := entries[target]; exists {
		return true
	}
	prefix := strings.TrimSuffix(target, "/") + "/"
	for name := range entries {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func assertAgentToolkitAdaptersResolve(
	t *testing.T,
	manifest agentToolkitManifest,
	entries map[string][]byte,
) {
	t.Helper()
	harnesses := make(map[string]bool)
	for _, adapter := range manifest.Adapters {
		harnesses[adapter.Harness] = true
		body, exists := entries[adapter.Path]
		if !exists {
			t.Errorf("%s adapter path %q does not resolve", adapter.Harness, adapter.Path)
			continue
		}
		if !bytes.Contains(body, []byte(".goobers/agent-toolkit/instructions/goobers.md")) {
			t.Errorf("%s adapter does not resolve the root instruction", adapter.Harness)
		}
		for _, skill := range adapter.Skills {
			relative := ".goobers/agent-toolkit/skills/" + skill + "/SKILL.md"
			if !bytes.Contains(body, []byte(relative)) {
				t.Errorf("%s adapter does not reference %s", adapter.Harness, skill)
			}
			if _, exists := entries["payload/"+relative]; !exists {
				t.Errorf("%s adapter references missing skill %s", adapter.Harness, skill)
			}
		}
	}
	for _, harness := range []string{"copilot", "claude"} {
		if !harnesses[harness] {
			t.Errorf("manifest is missing %s adapter", harness)
		}
	}
}

func assertAgentToolkitSafeContent(t *testing.T, repoRoot string, entries map[string][]byte) {
	t.Helper()
	secretPatterns := []*regexp.Regexp{
		regexp.MustCompile(`-----BEGIN (RSA |EC |OPENSSH )?PRIVATE KEY-----`),
		regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	}
	for path, data := range entries {
		if path != "manifest.json" {
			if !strings.HasPrefix(path, agentToolkitProductRoot+"/") {
				t.Errorf("asset %q is outside the product root", path)
			}
			if filepath.IsAbs(path) || strings.Contains(path, `\`) || hasParentSegment(path) {
				t.Errorf("asset path %q is not portable and relative", path)
			}
		}
		if bytes.Contains(data, []byte(repoRoot)) ||
			bytes.Contains(data, []byte("/Users/")) ||
			bytes.Contains(data, []byte(`C:\Users\`)) {
			t.Errorf("%s contains a machine-specific path", path)
		}
		for _, pattern := range secretPatterns {
			if pattern.Match(data) {
				t.Errorf("%s contains secret-shaped content matching %s", path, pattern)
			}
		}
	}
}

func hasParentSegment(path string) bool {
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

func readAgentToolkitArchive(t *testing.T, path string) map[string][]byte {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	entries := make(map[string][]byte, len(reader.File))
	for _, file := range reader.File {
		if _, duplicate := entries[file.Name]; duplicate {
			t.Fatalf("archive contains duplicate entry %q", file.Name)
		}
		stream, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(stream)
		closeErr := stream.Close()
		if err != nil {
			t.Fatal(err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
		entries[file.Name] = data
	}
	return entries
}

func agentToolkitRepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}
