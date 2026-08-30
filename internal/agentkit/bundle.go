// Package agentkit builds and manages the release-matched repository-side
// Goobers agent toolkit.
package agentkit

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/supportmatrix"
)

// Bundle and installation paths are versioned parts of the toolkit contract.
const (
	SchemaVersion = "1"
	BundleVersion = "1"
	ProductRoot   = "payload/.goobers/agent-toolkit"

	InstalledRoot         = ".goobers/agent-toolkit"
	InstalledManifestPath = InstalledRoot + "/manifest.json"
)

var skillNames = []string{
	"goobers-getting-started",
	"goobers-environment-resolver",
	"goobers-dsl-author",
	"goobers-run-operator",
	"goobers-workflow-upgrade",
}

// Manifest describes the release identity, ownership boundary, and assets.
type Manifest struct {
	Schema          string                  `json:"$schema"`
	SchemaVersion   string                  `json:"schemaVersion"`
	BundleVersion   string                  `json:"bundleVersion"`
	Producer        Producer                `json:"producer"`
	DSLVersions     []supportmatrix.Version `json:"dslVersions"`
	Ownership       Ownership               `json:"ownership"`
	Assets          []Asset                 `json:"assets"`
	Adapters        []Adapter               `json:"adapters"`
	CLICapabilities CLICapabilities         `json:"cliCapabilities"`
}

// Release is the installed release identity consumed by repository agents.
type Release struct {
	SchemaVersion string                  `json:"schemaVersion"`
	BundleVersion string                  `json:"bundleVersion"`
	Producer      Producer                `json:"producer"`
	DSLVersions   []supportmatrix.Version `json:"dslVersions"`
}

// Producer identifies the Goobers binary source that produced a bundle.
type Producer struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Ownership declares the product root and instruction files it excludes.
type Ownership struct {
	ProductRoot               string   `json:"productRoot"`
	UserOwnedInstructionFiles []string `json:"userOwnedInstructionFiles"`
}

// Asset records one product-owned file and its expected content.
type Asset struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode"`
}

// Adapter maps a supported harness to its toolkit references.
type Adapter struct {
	Harness           string   `json:"harness"`
	Path              string   `json:"path"`
	InstructionTarget string   `json:"instructionTarget"`
	Skills            []string `json:"skills"`
}

// CLICapabilities lists commands available to toolkit consumers.
type CLICapabilities struct {
	Required []string `json:"required"`
	Optional []string `json:"optional"`
}

// File is one embedded asset at its repository-relative installation path.
type File struct {
	Path string
	Data []byte
	Mode fs.FileMode
}

// Bundle contains the generated manifest and release-matched files.
type Bundle struct {
	Manifest     Manifest
	ManifestJSON []byte
	Files        map[string]File
}

// Build constructs a toolkit bundle from canonical source assets.
func Build(source fs.FS, version, commit string) (Bundle, error) {
	version = strings.TrimSpace(version)
	commit = strings.TrimSpace(commit)
	if version == "" || commit == "" {
		return Bundle{}, fmt.Errorf("agent toolkit producer version and commit must not be empty")
	}

	dslVersions := supportmatrix.GetDSL().Versions()
	producer := Producer{Version: version, Commit: commit}
	releaseJSON, err := json.MarshalIndent(Release{
		SchemaVersion: SchemaVersion,
		BundleVersion: BundleVersion,
		Producer:      producer,
		DSLVersions:   dslVersions,
	}, "", "  ")
	if err != nil {
		return Bundle{}, fmt.Errorf("encode agent toolkit release metadata: %w", err)
	}
	releaseJSON = append(releaseJSON, '\n')

	files := map[string]File{}
	add := func(destination string, data []byte, mode fs.FileMode) error {
		installedPath := InstalledRoot + "/" + strings.TrimPrefix(destination, "/")
		if _, duplicate := files[installedPath]; duplicate {
			return fmt.Errorf("duplicate agent toolkit asset %q", installedPath)
		}
		files[installedPath] = File{
			Path: installedPath,
			Data: append([]byte(nil), data...),
			Mode: mode,
		}
		return nil
	}
	if err := add("release.json", releaseJSON, 0o644); err != nil {
		return Bundle{}, err
	}

	singles := []struct {
		source      string
		destination string
	}{
		{"agent-toolkit/.gitattributes", ".gitattributes"},
		{"agent-toolkit/README.md", "README.md"},
		{"docs/ARCHITECTURE.md", "docs/ARCHITECTURE.md"},
		{"docs/VISION.md", "docs/VISION.md"},
		{"docs/adr/0001-agentic-sandbox-mechanism.md", "docs/adr/0001-agentic-sandbox-mechanism.md"},
		{"docs/adr/0002-provider-neutral-capability-namespaces.md", "docs/adr/0002-provider-neutral-capability-namespaces.md"},
		{"docs/design/cobrand.md", "docs/design/cobrand.md"},
		{"docs/design/notification-output.md", "docs/design/notification-output.md"},
		{"docs/design/static-fan-out-fan-in.md", "docs/design/static-fan-out-fan-in.md"},
		{"docs/guides/goobers-io-mcp.md", "docs/guides/goobers-io-mcp.md"},
		{"docs/guides/quickstart.md", "docs/guides/quickstart.md"},
		{"docs/guides/supervision.md", "docs/guides/supervision.md"},
		{"docs/stage-contract.md", "docs/stage-contract.md"},
		{"internal/capability/capability.go", "internal/capability/capability.go"},
	}
	for _, item := range singles {
		if err := addSourceFile(source, item.source, item.destination, add); err != nil {
			return Bundle{}, err
		}
	}

	trees := []struct {
		source      string
		destination string
		include     func(string) bool
	}{
		{"agent-toolkit/instructions", "instructions", includeAll},
		{"agent-toolkit/adapters", "adapters", includeAll},
		{"skills", "skills", includeAll},
		{"api/schemas", "api/schemas", func(name string) bool {
			return strings.HasSuffix(name, ".schema.json")
		}},
		{"config-examples", "config-examples", func(name string) bool {
			return !strings.HasSuffix(name, ".go")
		}},
		{"docs/requirements", "docs/requirements", includeAll},
	}
	for _, tree := range trees {
		if err := addSourceTree(source, tree.source, tree.destination, tree.include, add); err != nil {
			return Bundle{}, err
		}
	}

	assets := make([]Asset, 0, len(files))
	for installedPath, file := range files {
		sum := sha256.Sum256(file.Data)
		assets = append(assets, Asset{
			Path:   "payload/" + installedPath,
			SHA256: fmt.Sprintf("%x", sum),
			Size:   int64(len(file.Data)),
			Mode:   formatAssetMode(file.Mode),
		})
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].Path < assets[j].Path })

	manifest := Manifest{
		Schema:        schemas.BaseURI + schemas.AgentToolkitManifest,
		SchemaVersion: SchemaVersion,
		BundleVersion: BundleVersion,
		Producer:      producer,
		DSLVersions:   dslVersions,
		Ownership: Ownership{
			ProductRoot: ProductRoot,
			UserOwnedInstructionFiles: []string{
				"AGENTS.md",
				"CLAUDE.md",
				".github/copilot-instructions.md",
			},
		},
		Assets: assets,
		Adapters: []Adapter{
			newAdapter("copilot", ".github/copilot-instructions.md"),
			newAdapter("claude", "CLAUDE.md"),
			newAdapter("agents", "AGENTS.md"),
		},
		CLICapabilities: CLICapabilities{
			Required: []string{
				"version",
				"versions",
				"features",
				"examples list",
				"examples show",
				"validate",
				"status",
				"trace",
				"workflow show",
			},
			Optional: []string{
				"agent-kit install",
				"agent-kit check",
				"agent-kit update",
				"runs list",
				"stats",
				"blocked list",
				"claims list",
				"escalations show",
				"fix",
			},
		},
	}
	manifestJSON, err := marshalManifest(manifest)
	if err != nil {
		return Bundle{}, err
	}
	return Bundle{Manifest: manifest, ManifestJSON: manifestJSON, Files: files}, nil
}

// DecodeManifest validates and decodes installed manifest bytes.
func DecodeManifest(data []byte) (Manifest, error) {
	validator, err := validate.New()
	if err != nil {
		return Manifest{}, fmt.Errorf("create agent toolkit manifest validator: %w", err)
	}
	if err := validator.ValidateJSON(schemas.AgentToolkitManifest, data); err != nil {
		return Manifest{}, fmt.Errorf("validate agent toolkit manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode agent toolkit manifest: %w", err)
	}
	if err := validateManifestPaths(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// InstalledAssetPath converts a transport asset path to its repository path.
func InstalledAssetPath(assetPath string) (string, error) {
	const payloadPrefix = "payload/"
	if !strings.HasPrefix(assetPath, payloadPrefix) {
		return "", fmt.Errorf("agent toolkit asset %q is outside payload", assetPath)
	}
	installed := strings.TrimPrefix(assetPath, payloadPrefix)
	if installed != path.Clean(installed) ||
		!strings.HasPrefix(installed, InstalledRoot+"/") ||
		strings.Contains(installed, `\`) {
		return "", fmt.Errorf("agent toolkit asset %q has an unsafe path", assetPath)
	}
	return installed, nil
}

// Adapter returns the bundle adapter for a CLI harness name.
func (b Bundle) Adapter(harness string) (Adapter, bool) {
	manifestHarness := harness
	if harness == "generic" {
		manifestHarness = "agents"
	}
	for _, adapter := range b.Manifest.Adapters {
		if adapter.Harness == manifestHarness {
			return adapter, true
		}
	}
	return Adapter{}, false
}

func marshalManifest(manifest Manifest) ([]byte, error) {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode agent toolkit manifest: %w", err)
	}
	data = append(data, '\n')
	if _, err := DecodeManifest(data); err != nil {
		return nil, err
	}
	return data, nil
}

func validateManifestPaths(manifest Manifest) error {
	if manifest.Ownership.ProductRoot != ProductRoot {
		return fmt.Errorf("agent toolkit manifest product root %q is unsupported", manifest.Ownership.ProductRoot)
	}
	seen := make(map[string]struct{}, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		installedPath, err := InstalledAssetPath(asset.Path)
		if err != nil {
			return err
		}
		if installedPath == InstalledManifestPath {
			return fmt.Errorf("agent toolkit manifest must not claim itself as an asset")
		}
		if _, duplicate := seen[asset.Path]; duplicate {
			return fmt.Errorf("agent toolkit manifest contains duplicate asset %q", asset.Path)
		}
		if _, err := parseAssetMode(asset.Mode); err != nil {
			return fmt.Errorf("agent toolkit asset %q: %w", asset.Path, err)
		}
		seen[asset.Path] = struct{}{}
	}
	return nil
}

func newAdapter(harness, instructionTarget string) Adapter {
	return Adapter{
		Harness:           harness,
		Path:              ProductRoot + "/adapters/" + harness + ".md",
		InstructionTarget: instructionTarget,
		Skills:            append([]string(nil), skillNames...),
	}
}

func addSourceFile(
	source fs.FS,
	sourcePath, destination string,
	add func(string, []byte, fs.FileMode) error,
) error {
	data, err := fs.ReadFile(source, sourcePath)
	if err != nil {
		return fmt.Errorf("read agent toolkit source %s: %w", sourcePath, err)
	}
	return add(destination, data, SourceMode(sourcePath))
}

func addSourceTree(
	source fs.FS,
	sourceRoot, destinationRoot string,
	include func(string) bool,
	add func(string, []byte, fs.FileMode) error,
) error {
	err := fs.WalkDir(source, sourceRoot, func(sourcePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative := strings.TrimPrefix(strings.TrimPrefix(sourcePath, sourceRoot), "/")
		if !include(relative) {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("agent toolkit source %s is a symbolic link", sourcePath)
		}
		data, err := fs.ReadFile(source, sourcePath)
		if err != nil {
			return err
		}
		return add(path.Join(destinationRoot, relative), data, SourceMode(sourcePath))
	})
	if err != nil {
		return fmt.Errorf("collect agent toolkit source %s: %w", sourceRoot, err)
	}
	return nil
}

// SourceMode is the toolkit's canonical permission for a source file, keyed
// only by name convention rather than the host filesystem's reported mode.
// Both the go:embed-driven Build below and release/agent_toolkit.go's
// from-disk packaging path call this so the two produce byte-identical
// manifests: embed.FS never preserves real permission bits (embedded regular
// files always report as read-only), and on Windows os.Stat can't report a
// meaningful executable bit either (every regular file reads back as 0666).
// Trusting the OS's reported mode instead of this convention made the
// disk-read side diverge from the embedded side the moment either ran on
// Windows or read through embed.FS.
func SourceMode(sourcePath string) fs.FileMode {
	if strings.HasSuffix(sourcePath, ".sh") {
		return 0o755
	}
	return 0o644
}

func formatAssetMode(mode fs.FileMode) string {
	return fmt.Sprintf("%04o", mode.Perm())
}

func parseAssetMode(value string) (fs.FileMode, error) {
	mode, err := strconv.ParseUint(value, 8, 32)
	if err != nil || len(value) != 4 || value[0] != '0' || mode > 0o777 {
		return 0, fmt.Errorf("invalid file mode %q", value)
	}
	return fs.FileMode(mode), nil
}

func includeAll(string) bool { return true }
