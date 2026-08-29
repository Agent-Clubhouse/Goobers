package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/agentkit"
	"github.com/goobers/goobers/internal/supportmatrix"
)

const (
	agentToolkitSchemaVersion = "1"
	agentToolkitBundleVersion = "1"
	agentToolkitProductRoot   = "payload/.goobers/agent-toolkit"
	agentToolkitSchemaURI     = schemas.BaseURI + schemas.AgentToolkitManifest
)

var agentToolkitSkills = []string{
	"goobers-environment-resolver",
	"goobers-dsl-author",
	"goobers-run-operator",
	"goobers-workflow-upgrade",
}

type agentToolkitManifest struct {
	Schema          string                      `json:"$schema"`
	SchemaVersion   string                      `json:"schemaVersion"`
	BundleVersion   string                      `json:"bundleVersion"`
	Producer        agentToolkitProducer        `json:"producer"`
	DSLVersions     []supportmatrix.Version     `json:"dslVersions"`
	Ownership       agentToolkitOwnership       `json:"ownership"`
	Assets          []agentToolkitAsset         `json:"assets"`
	Adapters        []agentToolkitAdapter       `json:"adapters"`
	CLICapabilities agentToolkitCLICapabilities `json:"cliCapabilities"`
}

type agentToolkitRelease struct {
	SchemaVersion string                  `json:"schemaVersion"`
	BundleVersion string                  `json:"bundleVersion"`
	Producer      agentToolkitProducer    `json:"producer"`
	DSLVersions   []supportmatrix.Version `json:"dslVersions"`
}

type agentToolkitProducer struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type agentToolkitOwnership struct {
	ProductRoot               string   `json:"productRoot"`
	UserOwnedInstructionFiles []string `json:"userOwnedInstructionFiles"`
}

type agentToolkitAsset struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Mode   string `json:"mode"`
}

type agentToolkitAdapter struct {
	Harness           string   `json:"harness"`
	Path              string   `json:"path"`
	InstructionTarget string   `json:"instructionTarget"`
	Skills            []string `json:"skills"`
}

type agentToolkitCLICapabilities struct {
	Required []string `json:"required"`
	Optional []string `json:"optional"`
}

type agentToolkitPayloadAsset struct {
	agentToolkitAsset
	data []byte
	mode fs.FileMode
}

func packageAgentToolkit(repoRoot, version, commit, outDir string) (string, error) {
	version = strings.TrimSpace(version)
	commit = strings.TrimSpace(commit)
	if version == "" || commit == "" {
		return "", fmt.Errorf("agent toolkit producer version and commit must not be empty")
	}

	dslVersions := supportmatrix.GetDSL().Versions()
	producer := agentToolkitProducer{Version: version, Commit: commit}
	assets, err := collectAgentToolkitAssets(repoRoot, agentToolkitRelease{
		SchemaVersion: agentToolkitSchemaVersion,
		BundleVersion: agentToolkitBundleVersion,
		Producer:      producer,
		DSLVersions:   dslVersions,
	})
	if err != nil {
		return "", err
	}

	manifest := agentToolkitManifest{
		Schema:        agentToolkitSchemaURI,
		SchemaVersion: agentToolkitSchemaVersion,
		BundleVersion: agentToolkitBundleVersion,
		Producer:      producer,
		DSLVersions:   dslVersions,
		Ownership: agentToolkitOwnership{
			ProductRoot: agentToolkitProductRoot,
			UserOwnedInstructionFiles: []string{
				"AGENTS.md",
				"CLAUDE.md",
				".github/copilot-instructions.md",
			},
		},
		Assets: assetInventory(assets),
		Adapters: []agentToolkitAdapter{
			newAgentToolkitAdapter("copilot", ".github/copilot-instructions.md"),
			newAgentToolkitAdapter("claude", "CLAUDE.md"),
			newAgentToolkitAdapter("agents", "AGENTS.md"),
		},
		CLICapabilities: agentToolkitCLICapabilities{
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
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode agent toolkit manifest: %w", err)
	}
	manifestJSON = append(manifestJSON, '\n')
	validator, err := validate.New()
	if err != nil {
		return "", fmt.Errorf("create agent toolkit manifest validator: %w", err)
	}
	if err := validator.ValidateJSON(schemas.AgentToolkitManifest, manifestJSON); err != nil {
		return "", fmt.Errorf("validate agent toolkit manifest: %w", err)
	}

	archivePath := filepath.Join(outDir, agentToolkitArchiveName(version))
	if err := writeAgentToolkitZip(archivePath, manifestJSON, assets); err != nil {
		return "", err
	}
	return archivePath, nil
}

func agentToolkitArchiveName(version string) string {
	return fmt.Sprintf("goobers-agent-toolkit_%s.zip", version)
}

func newAgentToolkitAdapter(harness, instructionTarget string) agentToolkitAdapter {
	return agentToolkitAdapter{
		Harness:           harness,
		Path:              agentToolkitProductRoot + "/adapters/" + harness + ".md",
		InstructionTarget: instructionTarget,
		Skills:            append([]string(nil), agentToolkitSkills...),
	}
}

func collectAgentToolkitAssets(repoRoot string, release agentToolkitRelease) ([]agentToolkitPayloadAsset, error) {
	releaseJSON, err := json.MarshalIndent(release, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode agent toolkit release metadata: %w", err)
	}
	releaseJSON = append(releaseJSON, '\n')
	assets := []agentToolkitPayloadAsset{
		newAgentToolkitPayloadAsset(agentToolkitProductRoot+"/release.json", releaseJSON, 0o644),
	}

	fileSources := []struct {
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
	for _, source := range fileSources {
		asset, err := readAgentToolkitFile(repoRoot, source.source, source.destination)
		if err != nil {
			return nil, err
		}
		assets = append(assets, asset)
	}

	treeSources := []struct {
		source      string
		destination string
		include     func(string) bool
	}{
		{"agent-toolkit/instructions", "instructions", includeAgentToolkitFile},
		{"agent-toolkit/adapters", "adapters", includeAgentToolkitFile},
		{"skills", "skills", includeAgentToolkitFile},
		{"api/schemas", "api/schemas", func(path string) bool {
			return strings.HasSuffix(path, ".schema.json")
		}},
		{"config-examples", "config-examples", func(path string) bool {
			return !strings.HasSuffix(path, ".go")
		}},
		{"docs/requirements", "docs/requirements", includeAgentToolkitFile},
	}
	for _, source := range treeSources {
		treeAssets, err := readAgentToolkitTree(repoRoot, source.source, source.destination, source.include)
		if err != nil {
			return nil, err
		}
		assets = append(assets, treeAssets...)
	}

	sort.Slice(assets, func(i, j int) bool { return assets[i].Path < assets[j].Path })
	for i, asset := range assets {
		if i > 0 && assets[i-1].Path == asset.Path {
			return nil, fmt.Errorf("duplicate agent toolkit asset %q", asset.Path)
		}
		if !strings.HasPrefix(asset.Path, agentToolkitProductRoot+"/") {
			return nil, fmt.Errorf("agent toolkit asset %q is outside product root", asset.Path)
		}
	}
	return assets, nil
}

func includeAgentToolkitFile(string) bool {
	return true
}

func readAgentToolkitFile(repoRoot, source, destination string) (agentToolkitPayloadAsset, error) {
	sourcePath := filepath.Join(repoRoot, filepath.FromSlash(source))
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return agentToolkitPayloadAsset{}, fmt.Errorf("stat agent toolkit source %s: %w", source, err)
	}
	if !info.Mode().IsRegular() {
		return agentToolkitPayloadAsset{}, fmt.Errorf("agent toolkit source %s is not a regular file", source)
	}
	data, err := os.ReadFile(sourcePath)
	if err != nil {
		return agentToolkitPayloadAsset{}, fmt.Errorf("read agent toolkit source %s: %w", source, err)
	}
	return newAgentToolkitPayloadAsset(
		agentToolkitProductRoot+"/"+filepath.ToSlash(destination),
		data,
		agentkit.SourceMode(sourcePath),
	), nil
}

func readAgentToolkitTree(
	repoRoot, sourceDir, destinationDir string,
	include func(string) bool,
) ([]agentToolkitPayloadAsset, error) {
	root := filepath.Join(repoRoot, filepath.FromSlash(sourceDir))
	var assets []agentToolkitPayloadAsset
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !include(relative) {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("agent toolkit source %s/%s is a symbolic link", sourceDir, relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("agent toolkit source %s/%s is not a regular file", sourceDir, relative)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		assets = append(assets, newAgentToolkitPayloadAsset(
			agentToolkitProductRoot+"/"+destinationDir+"/"+relative,
			data,
			agentkit.SourceMode(path),
		))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("collect agent toolkit source %s: %w", sourceDir, err)
	}
	return assets, nil
}

func newAgentToolkitPayloadAsset(path string, data []byte, mode fs.FileMode) agentToolkitPayloadAsset {
	sum := sha256.Sum256(data)
	return agentToolkitPayloadAsset{
		agentToolkitAsset: agentToolkitAsset{
			Path:   path,
			SHA256: fmt.Sprintf("%x", sum),
			Size:   int64(len(data)),
			Mode:   fmt.Sprintf("%04o", mode.Perm()),
		},
		data: append([]byte(nil), data...),
		mode: mode,
	}
}

func assetInventory(assets []agentToolkitPayloadAsset) []agentToolkitAsset {
	inventory := make([]agentToolkitAsset, len(assets))
	for i, asset := range assets {
		inventory[i] = asset.agentToolkitAsset
	}
	return inventory
}

func writeAgentToolkitZip(path string, manifestJSON []byte, assets []agentToolkitPayloadAsset) (retErr error) {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create agent toolkit archive %s: %w", path, err)
	}
	defer func() {
		if err := file.Close(); retErr == nil && err != nil {
			retErr = fmt.Errorf("close agent toolkit archive %s: %w", path, err)
		}
		if retErr != nil {
			_ = os.Remove(path)
		}
	}()

	writer := zip.NewWriter(file)
	if err := writeAgentToolkitZipEntry(writer, "manifest.json", manifestJSON, 0o644); err != nil {
		_ = writer.Close()
		return fmt.Errorf("write agent toolkit manifest: %w", err)
	}
	for _, asset := range assets {
		if err := writeAgentToolkitZipEntry(writer, asset.Path, asset.data, asset.mode); err != nil {
			_ = writer.Close()
			return fmt.Errorf("write agent toolkit asset %s: %w", asset.Path, err)
		}
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close agent toolkit zip writer: %w", err)
	}
	return nil
}

func writeAgentToolkitZipEntry(writer *zip.Writer, name string, data []byte, mode fs.FileMode) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(mode)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = entry.Write(data)
	return err
}

func findAgentToolkitRepositoryRoot() (string, error) {
	root, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	for {
		goMod := filepath.Join(root, "go.mod")
		toolkit := filepath.Join(root, "agent-toolkit")
		if goModInfo, goModErr := os.Stat(goMod); goModErr == nil && !goModInfo.IsDir() {
			if toolkitInfo, toolkitErr := os.Stat(toolkit); toolkitErr == nil && toolkitInfo.IsDir() {
				return root, nil
			}
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", fmt.Errorf("find repository root containing go.mod and agent-toolkit")
		}
		root = parent
	}
}
