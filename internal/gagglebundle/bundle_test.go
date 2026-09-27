package gagglebundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"

	"sigs.k8s.io/yaml"
)

func TestExportDigestIsStableAndSanitized(t *testing.T) {
	source := newBundleSource(t)
	first, err := Export(source.ConfigDir(), "example", time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Export(source.ConfigDir(), "example", time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("unchanged definition digest moved: %s != %s", first.Digest, second.Digest)
	}
	if first.ExportedAt.Equal(second.ExportedAt) {
		t.Fatal("export timestamps unexpectedly equal")
	}
	if first.Source.Digest != first.Digest {
		t.Fatalf("source digest = %q, envelope digest = %q", first.Source.Digest, first.Digest)
	}
	gaggle := first.Definition.Gaggle
	if gaggle.Spec.SelfIdentity != "" || gaggle.Spec.OutboxMirrorPath != "" || gaggle.Spec.Workcopies != nil {
		t.Fatalf("destination-local gaggle fields survived: %+v", gaggle.Spec)
	}
	if got := gaggle.Spec.Isolation; got.Namespace != "gaggle-portable" || got.IdentityRef != "" {
		t.Fatalf("portable isolation = %+v", got)
	}
	if gaggle.Spec.Project.ConnectionRef != "" || gaggle.Spec.Backlog.ConnectionRef != "" {
		t.Fatal("connection references survived export")
	}
	if len(first.Definition.Files) < 3 {
		t.Fatalf("referenced instruction/skill files = %d, want at least 3", len(first.Definition.Files))
	}
	for _, file := range first.Definition.Files {
		if _, err := decodeAndValidateFile(file); err != nil {
			t.Fatalf("exported file %q invalid: %v", file.Path, err)
		}
	}
}

func TestExportRejectsExplicitEnvironmentValues(t *testing.T) {
	source := newBundleSource(t)
	path := filepath.Join(source.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "      run:\n", "      run:\n        env:\n          API_TOKEN: do-not-export\n", 1)
	if edited == string(raw) {
		t.Fatal("fixture workflow did not contain a run block")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(source.ConfigDir(), "example", time.Now()); !errors.Is(err, ErrInvalidBundle) || !strings.Contains(err.Error(), "run.env") {
		t.Fatalf("Export error = %v, want explicit run.env refusal", err)
	}
}

func TestCompanionContentPolicyRejectsExportAndImport(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{"provider token", []byte("token=ghp_" + strings.Repeat("a", 36))},
		{"private key", []byte("-----BEGIN PRIVATE KEY-----\nbody\n-----END PRIVATE KEY-----")},
		{"generic secret assignment", []byte("password=correct-horse-battery-staple")},
		{"credential URI", []byte("remote=https://alice:correct-horse@example.com/repo")},
		{"Windows local path", []byte(`workspace=C:\Users\alice\project`)},
		{"Unix local path", []byte("workspace=/home/alice/project")},
		{"invalid UTF-8 binary", []byte{0xff, 0xfe, 0x00}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newBundleSource(t)
			safe, err := Export(source.ConfigDir(), "example", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			instruction := companionInstructionPath(t, source)
			if err := os.WriteFile(instruction, test.data, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Export(source.ConfigDir(), "example", time.Now()); !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("Export error = %v, want ErrInvalidBundle", err)
			} else if bytes.Contains([]byte(err.Error()), test.data) {
				t.Fatal("export error echoed rejected companion content")
			}

			crafted := cloneBundle(t, safe)
			setBundleFileContent(t, &crafted, 0, test.data)
			if err := Validate(crafted); !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("Validate error = %v, want ErrInvalidBundle", err)
			} else if bytes.Contains([]byte(err.Error()), test.data) {
				t.Fatal("import validation error echoed rejected companion content")
			}
		})
	}
}

func TestCompanionContentPolicyAllowsPortableTemplate(t *testing.T) {
	source := newBundleSource(t)
	content := []byte("# Portable template\nTOKEN=${GITHUB_TOKEN}\npassword={{PASSWORD}}\nworkspace=./checkout\n")
	if err := os.WriteFile(companionInstructionPath(t, source), content, 0o644); err != nil {
		t.Fatal(err)
	}
	bundle, err := Export(source.ConfigDir(), "example", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(bundle); err != nil {
		t.Fatal(err)
	}
}

func TestStructuredCredentialPolicyRejectsExportAndImport(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 36)
	tests := []struct {
		name           string
		mutateSource   func(*testing.T, instance.Layout)
		mutateImported func(*apiv1.GaggleBundle)
	}{
		{
			name: "deterministic command",
			mutateSource: func(t *testing.T, source instance.Layout) {
				mutateSourceWorkflow(t, source, func(workflow *apiv1.Workflow) {
					workflow.Spec.Tasks[0].Run.Command = []string{"tool", "--token=" + secret}
				})
			},
			mutateImported: func(bundle *apiv1.GaggleBundle) {
				bundle.Definition.Workflows[0].Spec.Tasks[0].Run.Command = []string{"tool", "--token=" + secret}
			},
		},
		{
			name: "deterministic script",
			mutateSource: func(t *testing.T, source instance.Layout) {
				mutateSourceWorkflow(t, source, func(workflow *apiv1.Workflow) {
					workflow.Spec.Tasks[0].Run.Command = nil
					workflow.Spec.Tasks[0].Run.Script = "TOKEN=" + secret
				})
			},
			mutateImported: func(bundle *apiv1.GaggleBundle) {
				bundle.Definition.Workflows[0].Spec.Tasks[0].Run.Command = nil
				bundle.Definition.Workflows[0].Spec.Tasks[0].Run.Script = "TOKEN=" + secret
			},
		},
		{
			name: "MCP arguments",
			mutateSource: func(t *testing.T, source instance.Layout) {
				mutateSourceGoober(t, source, func(goober *apiv1.Goober) {
					goober.Spec.MCPServers = []apiv1.MCPServer{{Name: "leaked", Command: "tool", Args: []string{"--token=" + secret}}}
				})
			},
			mutateImported: func(bundle *apiv1.GaggleBundle) {
				bundle.Definition.Goobers[0].Spec.MCPServers = []apiv1.MCPServer{{Name: "leaked", Command: "tool", Args: []string{"--token=" + secret}}}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newBundleSource(t)
			safe, err := Export(source.ConfigDir(), "example", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			test.mutateSource(t, source)
			if _, err := Export(source.ConfigDir(), "example", time.Now()); !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("Export error = %v, want ErrInvalidBundle", err)
			} else if strings.Contains(err.Error(), secret) {
				t.Fatal("export error echoed rejected credential")
			}

			crafted := cloneBundle(t, safe)
			test.mutateImported(&crafted)
			refreshBundleDigest(t, &crafted)
			if err := Validate(crafted); !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("Validate error = %v, want ErrInvalidBundle", err)
			} else if strings.Contains(err.Error(), secret) {
				t.Fatal("import validation error echoed rejected credential")
			}
		})
	}
}

func TestCompanionLimitsRejectExportAndImport(t *testing.T) {
	tests := []struct {
		name  string
		files int
		size  int
	}{
		{name: "per file", files: 1, size: maxCompanionFileBytes + 1},
		{name: "aggregate", files: 9, size: 1 << 20},
		{name: "count", files: maxCompanionFiles + 1, size: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := newBundleSource(t)
			safe, err := Export(source.ConfigDir(), "example", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			skillDir, skill := companionSkillDir(t, source)
			if err := os.RemoveAll(skillDir); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(skillDir, 0o755); err != nil {
				t.Fatal(err)
			}
			content := bytes.Repeat([]byte("a"), test.size)
			for i := range test.files {
				if err := os.WriteFile(filepath.Join(skillDir, fmt.Sprintf("file-%03d.txt", i)), content, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Export(source.ConfigDir(), "example", time.Now()); !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("Export error = %v, want ErrInvalidBundle", err)
			}

			crafted := cloneBundle(t, safe)
			crafted.Definition.Files = nil
			for i := range test.files {
				file := apiv1.GaggleBundleFile{Path: fmt.Sprintf("skills/%s/file-%03d.txt", skill, i)}
				setFileContent(&file, content)
				crafted.Definition.Files = append(crafted.Definition.Files, file)
			}
			refreshBundleDigest(t, &crafted)
			if err := Validate(crafted); !errors.Is(err, ErrInvalidBundle) {
				t.Fatalf("Validate error = %v, want ErrInvalidBundle", err)
			}
		})
	}
}

func TestExportPackagesSharedSkillForPortableImport(t *testing.T) {
	source := newBundleSource(t)
	set, report, err := instance.LoadConfigDir(source.ConfigDir())
	if err != nil {
		t.Fatalf("load source: %v report=%+v", err, report)
	}
	var skill string
	for _, goober := range set.Goobers {
		if goober.Spec.Gaggle == "example" && len(goober.Spec.Skills) != 0 {
			skill = goober.Spec.Skills[0]
			break
		}
	}
	if skill == "" {
		t.Fatal("example fixture has no referenced skill")
	}
	scoped := filepath.Join(source.ConfigDir(), "gaggles", "example", "skills", skill)
	shared := filepath.Join(source.Root, "skills", skill)
	if err := os.MkdirAll(filepath.Dir(shared), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(scoped, shared); err != nil {
		t.Fatal(err)
	}
	bundle, err := Export(source.ConfigDir(), "example", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	prefix := "skills/" + skill + "/"
	found := false
	for _, file := range bundle.Definition.Files {
		found = found || strings.HasPrefix(file.Path, prefix)
	}
	if !found {
		t.Fatalf("shared skill %q was not packaged under %q", skill, prefix)
	}
	destination := newEmptyBundleDestination(t, source)
	swap, err := PrepareImport(destination, "copied-example", bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := swap.Commit(); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(destination.ConfigDir(), "gaggles", "copied-example", "skills", skill)); err != nil || !info.IsDir() {
		t.Fatalf("imported skill package missing: info=%v err=%v", info, err)
	}
}

func TestPrepareImportRoundTripsAndRetainsProvenance(t *testing.T) {
	source := newBundleSource(t)
	sourceManifest := readFile(t, filepath.Join(source.ConfigDir(), "manifest.yaml"))
	sourceGaggle := readFile(t, filepath.Join(source.ConfigDir(), "gaggles", "example", "gaggle.yaml"))
	bundle, err := Export(source.ConfigDir(), "example", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	destination := newEmptyBundleDestination(t, source)
	swap, err := PrepareImport(destination, "copied-example", bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := swap.Commit(); err != nil {
		t.Fatal(err)
	}
	set, report, err := instance.LoadConfigDir(destination.ConfigDir())
	if err != nil {
		t.Fatalf("LoadConfigDir: %v report=%+v", err, report)
	}
	if len(set.Gaggles) != 1 || set.Gaggles[0].Name != "copied-example" {
		t.Fatalf("imported gaggles = %+v", set.Gaggles)
	}
	if set.Gaggles[0].Spec.Isolation.Namespace != "gaggle-copied-example" {
		t.Fatalf("destination isolation = %+v", set.Gaggles[0].Spec.Isolation)
	}
	for _, workflow := range set.Workflows {
		if workflow.Spec.Gaggle != "copied-example" {
			t.Fatalf("workflow %q gaggle = %q", workflow.Name, workflow.Spec.Gaggle)
		}
	}
	for _, goober := range set.Goobers {
		if goober.Spec.Gaggle != "copied-example" {
			t.Fatalf("goober %q gaggle = %q", goober.Name, goober.Spec.Gaggle)
		}
	}
	provenance, err := os.ReadFile(filepath.Join(destination.ConfigDir(), "gaggles", "copied-example", "bundle-source.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(provenance, []byte(bundle.Digest)) || !bytes.Contains(provenance, []byte(`"name": "example"`)) {
		t.Fatalf("retained provenance = %s", provenance)
	}
	if after := readFile(t, filepath.Join(source.ConfigDir(), "manifest.yaml")); !bytes.Equal(sourceManifest, after) {
		t.Fatal("import mutated the source manifest")
	}
	if after := readFile(t, filepath.Join(source.ConfigDir(), "gaggles", "example", "gaggle.yaml")); !bytes.Equal(sourceGaggle, after) {
		t.Fatal("import mutated the source gaggle")
	}
}

func TestPrepareImportValidatesBeforeMutation(t *testing.T) {
	source := newBundleSource(t)
	base, err := Export(source.ConfigDir(), "example", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*apiv1.GaggleBundle)
		want   error
	}{
		{
			name: "invalid schema",
			mutate: func(bundle *apiv1.GaggleBundle) {
				bundle.SchemaVersion = 99
			},
			want: ErrInvalidBundle,
		},
		{
			name: "invalid reference",
			mutate: func(bundle *apiv1.GaggleBundle) {
				bundle.Definition.Workflows[0].Spec.Gaggle = "missing"
				digest, digestErr := DefinitionDigest(bundle.Definition)
				if digestErr != nil {
					t.Fatal(digestErr)
				}
				bundle.Digest = digest
				bundle.Source.Digest = digest
			},
			want: ErrInvalidBundle,
		},
		{
			name: "unreferenced companion file",
			mutate: func(bundle *apiv1.GaggleBundle) {
				bundle.Definition.Files[0].Path = "gaggle.yaml"
				refreshBundleDigest(t, bundle)
			},
			want: ErrInvalidBundle,
		},
		{
			name: "omitted repository authorization requirement",
			mutate: func(bundle *apiv1.GaggleBundle) {
				bundle.Definition.Repositories = nil
				refreshBundleDigest(t, bundle)
			},
			want: ErrInvalidBundle,
		},
		{
			name: "sibling repository connection reference",
			mutate: func(bundle *apiv1.GaggleBundle) {
				sibling := apiv1.GaggleSibling{Label: "sibling"}
				sibling.Project = bundle.Definition.Gaggle.Spec.Project
				sibling.Project.Name = "sibling-repo"
				sibling.Project.ConnectionRef = "source-credential"
				bundle.Definition.Gaggle.Spec.Siblings = append(bundle.Definition.Gaggle.Spec.Siblings, sibling)
				refreshBundleDigest(t, bundle)
			},
			want: ErrInvalidBundle,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			destination := newEmptyBundleDestination(t, source)
			before := readFile(t, filepath.Join(destination.ConfigDir(), "manifest.yaml"))
			bundle := cloneBundle(t, base)
			test.mutate(&bundle)
			if _, err := PrepareImport(destination, "copied-example", bundle); !errors.Is(err, test.want) {
				t.Fatalf("PrepareImport error = %v, want %v", err, test.want)
			}
			after := readFile(t, filepath.Join(destination.ConfigDir(), "manifest.yaml"))
			if !bytes.Equal(before, after) {
				t.Fatal("invalid bundle mutated destination manifest")
			}
			if _, err := os.Stat(filepath.Join(destination.ConfigDir(), "gaggles", "copied-example")); !os.IsNotExist(err) {
				t.Fatalf("invalid bundle left partial gaggle: %v", err)
			}
		})
	}
}

func cloneBundle(t *testing.T, bundle apiv1.GaggleBundle) apiv1.GaggleBundle {
	t.Helper()
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	var clone apiv1.GaggleBundle
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return clone
}

func refreshBundleDigest(t *testing.T, bundle *apiv1.GaggleBundle) {
	t.Helper()
	digest, err := DefinitionDigest(bundle.Definition)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Digest = digest
	bundle.Source.Digest = digest
}

func setBundleFileContent(t *testing.T, bundle *apiv1.GaggleBundle, index int, data []byte) {
	t.Helper()
	setFileContent(&bundle.Definition.Files[index], data)
	refreshBundleDigest(t, bundle)
}

func setFileContent(file *apiv1.GaggleBundleFile, data []byte) {
	sum := sha256.Sum256(data)
	file.ContentBase64 = base64.StdEncoding.EncodeToString(data)
	file.SHA256 = fmt.Sprintf("sha256:%x", sum)
}

func companionInstructionPath(t *testing.T, layout instance.Layout) string {
	t.Helper()
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		t.Fatalf("load source: %v report=%+v", err, report)
	}
	for _, goober := range set.Goobers {
		if goober.Spec.Gaggle != "example" || goober.Spec.Instructions == "" {
			continue
		}
		source, ok := set.GooberSource(goober.Name)
		if !ok {
			t.Fatalf("missing source for goober %q", goober.Name)
		}
		return filepath.Join(layout.ConfigDir(), filepath.FromSlash(filepath.Dir(source)), filepath.FromSlash(goober.Spec.Instructions))
	}
	t.Fatal("example fixture has no Goober instructions")
	return ""
}

func companionSkillDir(t *testing.T, layout instance.Layout) (string, string) {
	t.Helper()
	set, report, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		t.Fatalf("load source: %v report=%+v", err, report)
	}
	for _, goober := range set.Goobers {
		if goober.Spec.Gaggle == "example" && len(goober.Spec.Skills) != 0 {
			skill := goober.Spec.Skills[0]
			return filepath.Join(layout.ConfigDir(), "gaggles", "example", "skills", skill), skill
		}
	}
	t.Fatal("example fixture has no referenced skill")
	return "", ""
}

func TestPrepareImportRejectsNameConflictAndMissingAuthorization(t *testing.T) {
	source := newBundleSource(t)
	bundle, err := Export(source.ConfigDir(), "example", time.Now())
	if err != nil {
		t.Fatal(err)
	}

	t.Run("name conflict", func(t *testing.T) {
		destination := newEmptyBundleDestination(t, source)
		swap, err := PrepareImport(destination, "copied-example", bundle)
		if err != nil {
			t.Fatal(err)
		}
		if err := swap.Commit(); err != nil {
			t.Fatal(err)
		}
		if _, err := PrepareImport(destination, "copied-example", bundle); !errors.Is(err, ErrNameConflict) {
			t.Fatalf("second import error = %v, want name conflict", err)
		}
	})

	t.Run("missing repository authorization", func(t *testing.T) {
		destination := newEmptyBundleDestination(t, source)
		cfg, err := instance.LoadConfig(destination.ConfigFile())
		if err != nil {
			t.Fatal(err)
		}
		cfg.Repos = nil
		if err := instance.WriteConfig(destination.ConfigFile(), cfg); err != nil {
			t.Fatal(err)
		}
		before := readFile(t, filepath.Join(destination.ConfigDir(), "manifest.yaml"))
		if _, err := PrepareImport(destination, "copied-example", bundle); !errors.Is(err, ErrRepositoryAuthorization) {
			t.Fatalf("PrepareImport error = %v, want authorization requirement", err)
		}
		if after := readFile(t, filepath.Join(destination.ConfigDir(), "manifest.yaml")); !bytes.Equal(before, after) {
			t.Fatal("authorization failure mutated destination")
		}
	})
}

func TestPrepareImportWriteFailureLeavesDestinationUnchanged(t *testing.T) {
	source := newBundleSource(t)
	bundle, err := Export(source.ConfigDir(), "example", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	destination := newEmptyBundleDestination(t, source)
	before := readFile(t, filepath.Join(destination.ConfigDir(), "manifest.yaml"))
	original := prepareConfigDirSwap
	prepareConfigDirSwap = func(instance.Layout, string) (*instance.PreparedConfigSwap, error) {
		return nil, errors.New("injected write failure")
	}
	defer func() { prepareConfigDirSwap = original }()
	if _, err := PrepareImport(destination, "copied-example", bundle); err == nil || !strings.Contains(err.Error(), "injected write failure") {
		t.Fatalf("PrepareImport error = %v", err)
	}
	if after := readFile(t, filepath.Join(destination.ConfigDir(), "manifest.yaml")); !bytes.Equal(before, after) {
		t.Fatal("write failure mutated destination")
	}
	if _, err := os.Stat(filepath.Join(destination.ConfigDir(), "gaggles", "copied-example")); !os.IsNotExist(err) {
		t.Fatalf("write failure left partial gaggle: %v", err)
	}
}

func newBundleSource(t *testing.T) instance.Layout {
	t.Helper()
	root := filepath.Join(t.TempDir(), "source")
	if _, err := instance.Init(root); err != nil {
		t.Fatal(err)
	}
	return instance.NewLayout(root)
}

func newEmptyBundleDestination(t *testing.T, source instance.Layout) instance.Layout {
	t.Helper()
	root := filepath.Join(t.TempDir(), "destination")
	layout := instance.NewLayout(root)
	if err := os.MkdirAll(layout.ConfigDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	instanceConfig := readFile(t, source.ConfigFile())
	if err := os.WriteFile(layout.ConfigFile(), instanceConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	rawManifest := readFile(t, filepath.Join(source.ConfigDir(), "manifest.yaml"))
	var manifest apiv1.Manifest
	if err := yaml.UnmarshalStrict(rawManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Spec.Gaggles = nil
	data, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(layout.ConfigDir(), "manifest.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, report, err := instance.LoadConfigDir(layout.ConfigDir()); err != nil {
		t.Fatalf("empty destination invalid: %v report=%+v", err, report)
	}
	return layout
}

func mutateSourceWorkflow(t *testing.T, source instance.Layout, mutate func(*apiv1.Workflow)) {
	t.Helper()
	path := filepath.Join(source.ConfigDir(), "gaggles", "example", "workflows", "default-implement.yaml")
	var workflow apiv1.Workflow
	if err := yaml.UnmarshalStrict(readFile(t, path), &workflow); err != nil {
		t.Fatal(err)
	}
	mutate(&workflow)
	data, err := yaml.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mutateSourceGoober(t *testing.T, source instance.Layout, mutate func(*apiv1.Goober)) {
	t.Helper()
	path := filepath.Join(source.ConfigDir(), "gaggles", "example", "goobers", "coder", "goober.yaml")
	var goober apiv1.Goober
	if err := yaml.UnmarshalStrict(readFile(t, path), &goober); err != nil {
		t.Fatal(err)
	}
	mutate(&goober)
	data, err := yaml.Marshal(goober)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
