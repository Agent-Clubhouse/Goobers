package instance

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// starterFS embeds the valid configuration templates that init can seed into a
// freshly initialized instance's config/ dir.
//
//go:embed starter demo quickstart-v1
var starterFS embed.FS

const (
	starterDir    = "starter"
	demoDir       = "demo"
	quickstartDir = "quickstart-v1"

	// gooberFileName is the goober spec file name inside a seeded template.
	gooberFileName = "goober.yaml"

	// QuickstartTemplate is the public init selector for the embedded
	// onboarding-only quickstart@v1 configuration.
	QuickstartTemplate = "quickstart"
)

// InitResult reports what Init created vs. left alone.
type InitResult struct {
	Root    string
	Created []string
	Skipped []string
}

// TargetConflictError marks an init refusal caused by pre-existing content at
// the target path, so callers can attach invocation-specific hints (for
// example, that the target defaulted to the current directory).
type TargetConflictError struct {
	msg string
}

func (e *TargetConflictError) Error() string { return e.msg }

func targetConflictf(format string, args ...any) error {
	return &TargetConflictError{msg: fmt.Sprintf(format, args...)}
}

// ConfigSourceSeedResult reports the config source files that were created or
// preserved.
type ConfigSourceSeedResult struct {
	Root    string
	Created []string
	Skipped []string
}

// Init scaffolds an instance root at root: instance.yaml, config/ (seeded
// with a starter example), gaggles/, scheduler/, and a
// telemetry.db placeholder (INST-010, ARCHITECTURE.md §6).
//
// Init is idempotent and non-destructive: any piece that already exists is
// left untouched and reported under Skipped, so a repeated `goobers init`
// never clobbers user edits (INST-008).
func Init(root string) (*InitResult, error) {
	return initWithConfig(root, starterDir, defaultConfig())
}

// InitDemo scaffolds a credential-free instance with one runnable,
// deterministic full-loop demo workflow backed by a hermetic mock provider.
func InitDemo(root string) (*InitResult, error) {
	return initWithConfig(root, demoDir, demoConfig())
}

// QuickstartOptions carries the non-interactive choices init may apply to the
// embedded quickstart template.
type QuickstartOptions struct {
	// Harness overrides the agent harness every generated agentic goober
	// uses (apiv1.HarnessCopilot or apiv1.HarnessClaudeCode). Empty keeps
	// the harness the template itself declares, so the default seeding is
	// byte-identical to the embedded template.
	Harness string
}

// InitQuickstart scaffolds the versioned onboarding template: one linear
// backlog-to-PR workflow with no production remediation or escalation paths.
func InitQuickstart(root string) (*InitResult, error) {
	return InitQuickstartWithOptions(root, QuickstartOptions{})
}

// InitQuickstartWithOptions is InitQuickstart with the template choices in
// opts applied to the seeded configuration.
func InitQuickstartWithOptions(root string, opts QuickstartOptions) (*InitResult, error) {
	files, err := quickstartTemplateFiles(opts)
	if err != nil {
		return nil, err
	}
	return initWithSeed(root, defaultConfig(), func(dir string) error {
		return writeConfigFiles(dir, files)
	})
}

// SeedQuickstartConfigSource creates the checked-in form of the quickstart
// template without runtime state. Identical files are preserved; conflicting
// managed paths are rejected.
func SeedQuickstartConfigSource(root string) (*ConfigSourceSeedResult, error) {
	return SeedQuickstartConfigSourceWithOptions(root, QuickstartOptions{})
}

// SeedQuickstartConfigSourceWithOptions is SeedQuickstartConfigSource with the
// template choices in opts applied to the seeded configuration.
func SeedQuickstartConfigSourceWithOptions(root string, opts QuickstartOptions) (*ConfigSourceSeedResult, error) {
	if root == "" {
		return nil, errors.New("config source path is required")
	}
	config, err := marshalConfig(defaultConfig())
	if err != nil {
		return nil, err
	}
	files := []configSeedFile{{
		path: GuidedSourceInstanceFile,
		data: config,
	}}
	templateFiles, err := quickstartTemplateFiles(opts)
	if err != nil {
		return nil, err
	}
	files = append(files, templateFiles...)
	return seedConfigSource(root, files, "quickstart template")
}

// quickstartTemplateFiles loads the embedded quickstart tree and applies the
// selected template options to it.
func quickstartTemplateFiles(opts QuickstartOptions) ([]configSeedFile, error) {
	files, err := embeddedConfigFiles(quickstartDir)
	if err != nil {
		return nil, fmt.Errorf("load quickstart template: %w", err)
	}
	if err := applyQuickstartHarness(files, opts.Harness); err != nil {
		return nil, err
	}
	return files, nil
}

// ValidateQuickstartHarness reports whether harness names a harness the
// quickstart template can be seeded with. Empty selects the template default.
func ValidateQuickstartHarness(harness string) error {
	switch apiv1.Harness(harness) {
	case "", apiv1.HarnessCopilot, apiv1.HarnessClaudeCode:
		return nil
	}
	return fmt.Errorf("harness must be %q or %q", apiv1.HarnessCopilot, apiv1.HarnessClaudeCode)
}

// quickstartHarnessLine matches the `harness:` key of an embedded goober spec.
// The template files are product-owned and comment-free, so rewriting the line
// in place keeps the seeded YAML byte-identical to the template apart from the
// selected harness — a full decode/encode round trip would reflow every
// generated goober.yaml instead.
var quickstartHarnessLine = regexp.MustCompile(`(?m)^([ \t]*harness:)[ \t]*\S.*$`)

func applyQuickstartHarness(files []configSeedFile, harness string) error {
	if err := ValidateQuickstartHarness(harness); err != nil {
		return err
	}
	if harness == "" {
		return nil
	}
	for i, file := range files {
		if path.Base(file.path) != gooberFileName {
			continue
		}
		rewritten, count := replaceQuickstartHarness(file.data, harness)
		if count != 1 {
			return fmt.Errorf("select harness %q: quickstart template %s declares %d harness keys, want exactly 1", harness, file.path, count)
		}
		files[i].data = rewritten
	}
	return nil
}

func replaceQuickstartHarness(data []byte, harness string) ([]byte, int) {
	matches := quickstartHarnessLine.FindAllIndex(data, -1)
	if len(matches) != 1 {
		return data, len(matches)
	}
	return quickstartHarnessLine.ReplaceAll(data, []byte("${1} "+harness)), 1
}

func seedConfigSource(root string, files []configSeedFile, templateName string) (*ConfigSourceSeedResult, error) {
	root = filepath.Clean(root)
	if err := prepareConfigSourceRoot(root); err != nil {
		return nil, err
	}
	result := &ConfigSourceSeedResult{
		Root:    root,
		Created: []string{},
		Skipped: []string{},
	}
	existing := make([]bool, len(files))
	for i, file := range files {
		var err error
		existing[i], err = inspectConfigSeedFile(root, file, templateName)
		if err != nil {
			return nil, fmt.Errorf("seed config source %s: %w", root, err)
		}
		if existing[i] {
			result.Skipped = append(result.Skipped, file.path)
		}
	}
	for i, file := range files {
		if existing[i] {
			continue
		}
		if err := writeConfigSeedFile(root, file); err != nil {
			return nil, fmt.Errorf("seed config source %s: %w", root, err)
		}
		result.Created = append(result.Created, file.path)
	}
	if _, report, err := LoadConfigDir(root); err != nil {
		return nil, fmt.Errorf("validate seeded config source %s: %w (report: %+v)", root, err, report)
	}
	return result, nil
}

func initWithConfig(root, configSource string, cfg *Config) (*InitResult, error) {
	return initWithSeed(root, cfg, func(dir string) error {
		return copyConfig(dir, configSource)
	})
}

func initWithSeed(root string, cfg *Config, seedConfig func(string) error) (*InitResult, error) {
	l := NewLayout(root)
	res := &InitResult{Root: root}

	if err := checkInitTarget(l); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create instance root %s: %w", root, err)
	}

	if exists(l.ConfigFile()) {
		res.Skipped = append(res.Skipped, ConfigFileName)
	} else {
		if err := WriteConfig(l.ConfigFile(), cfg); err != nil {
			return nil, err
		}
		res.Created = append(res.Created, ConfigFileName)
	}

	configSeeded, err := dirHasFiles(l.ConfigDir())
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", ConfigDirName, err)
	}
	if configSeeded {
		res.Skipped = append(res.Skipped, ConfigDirName)
	} else {
		if err := seedConfig(l.ConfigDir()); err != nil {
			return nil, fmt.Errorf("seed %s: %w", ConfigDirName, err)
		}
		res.Created = append(res.Created, ConfigDirName)
	}

	for _, name := range []string{GagglesDirName, SchedulerDirName} {
		dir := filepath.Join(root, name)
		if exists(dir) {
			res.Skipped = append(res.Skipped, name)
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", name, err)
		}
		res.Created = append(res.Created, name)
	}

	if exists(l.TelemetryDB()) {
		res.Skipped = append(res.Skipped, TelemetryDBName)
	} else {
		if err := os.WriteFile(l.TelemetryDB(), nil, 0o644); err != nil {
			return nil, fmt.Errorf("create %s: %w", TelemetryDBName, err)
		}
		res.Created = append(res.Created, TelemetryDBName)
	}

	return res, nil
}

// defaultConfig is the instance.yaml written by a fresh Init: a single
// placeholder repo entry the user is expected to edit, with a structurally
// valid (env-based) token ref so `goobers validate` passes with no ambient
// credentials required.
func defaultConfig() *Config {
	return &Config{
		APIVersion: ConfigAPIVersion,
		Kind:       ConfigKind,
		Repos: []RepoRef{
			{
				Provider: string(apiv1.ProviderGitHub),
				Owner:    "your-org",
				Name:     "your-repo",
				Token:    TokenRef{Env: "GOOBERS_GITHUB_TOKEN"},
			},
		},
		RunConditions: RunConditions{MaxParallelRuns: 1},
	}
}

func demoConfig() *Config {
	return &Config{
		APIVersion:    ConfigAPIVersion,
		Kind:          ConfigKind,
		RunConditions: RunConditions{MaxParallelRuns: 1},
	}
}

// checkInitTarget refuses a first-run init whose config/ already contains
// files that are not Goobers instance config — most commonly the target
// defaulted to a source checkout of this repository, whose tracked config/
// holds CRD manifests (#2513). Nothing is written on refusal. Re-runs over an
// existing instance (instance.yaml present) and config-first layouts (config/
// carries a Manifest) stay allowed, preserving idempotent init (INST-008).
func checkInitTarget(l Layout) error {
	if exists(l.ConfigFile()) {
		return nil
	}
	populated, err := dirHasFiles(l.ConfigDir())
	if err != nil {
		return fmt.Errorf("inspect %s: %w", ConfigDirName, err)
	}
	if !populated {
		return nil
	}
	hasManifest, err := configDirHasManifest(l.ConfigDir())
	if err != nil {
		return fmt.Errorf("inspect %s: %w", ConfigDirName, err)
	}
	if hasManifest {
		return nil
	}
	return targetConflictf("refusing to initialize %s: %s already contains files, but none is Goobers config (no `kind: Manifest` document) — the target looks like an unrelated project (for example a Goobers source checkout, whose config/ holds CRD manifests); choose an empty directory instead, e.g. `goobers init ./my-instance`", absPath(l.Root), ConfigDirName)
}

// configDirHasManifest reports whether any YAML document under dir declares
// kind: Manifest — the marker distinguishing an instance config directory from
// an unrelated directory that happens to be named config/.
func configDirHasManifest(dir string) (bool, error) {
	docs, err := readDocs(dir)
	if err != nil {
		return false, err
	}
	for _, doc := range docs {
		if doc.kind == "Manifest" {
			return true, nil
		}
	}
	return false, nil
}

// absPath resolves path for display, falling back to the raw path when
// resolution fails.
func absPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// dirHasFiles reports whether dir exists and already contains entries. A
// missing dir is treated as empty, not an error.
func dirHasFiles(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return len(entries) > 0, nil
}

type configSeedFile struct {
	path string
	data []byte
}

func embeddedConfigFiles(source string) ([]configSeedFile, error) {
	var files []configSeedFile
	err := fs.WalkDir(starterFS, source, func(filePath string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(source, filePath)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		data, err := starterFS.ReadFile(filePath)
		if err != nil {
			return err
		}
		files = append(files, configSeedFile{path: rel, data: data})
		return nil
	})
	return files, err
}

// copyConfig extracts one embedded config tree into dir.
func copyConfig(dir, source string) error {
	files, err := embeddedConfigFiles(source)
	if err != nil {
		return err
	}
	return writeConfigFiles(dir, files)
}

// writeConfigFiles writes a seeded config tree into dir.
func writeConfigFiles(dir string, files []configSeedFile) error {
	for _, file := range files {
		target := filepath.Join(dir, filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, file.data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func prepareConfigSourceRoot(root string) error {
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("create config source %s: %w", root, err)
		}
		info, err = os.Lstat(root)
	}
	if err != nil {
		return fmt.Errorf("inspect config source %s: %w", root, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("config source path %s must not be a symlink", root)
	}
	if !info.IsDir() {
		return fmt.Errorf("config source path %s is not a directory", root)
	}
	return nil
}

func inspectConfigSeedFile(root string, file configSeedFile, templateName string) (bool, error) {
	rel := filepath.FromSlash(file.path)
	if !filepath.IsLocal(rel) {
		return false, fmt.Errorf("managed path %s is not relative to the config source", file.path)
	}
	if err := inspectConfigSeedParents(root, filepath.Dir(rel)); err != nil {
		return false, fmt.Errorf("inspect parent for %s: %w", file.path, err)
	}
	target := filepath.Join(root, rel)
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect %s: %w", file.path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("managed path %s must not be a symlink", file.path)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("managed path %s must be a regular file", file.path)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", file.path, err)
	}
	if !bytes.Equal(data, file.data) {
		return false, fmt.Errorf("managed file %s differs from the %s; refusing to overwrite", file.path, templateName)
	}
	return true, nil
}

func inspectConfigSeedParents(root, relDir string) error {
	if relDir == "." {
		return nil
	}
	current := root
	for _, part := range strings.Split(relDir, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s must not be a symlink", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", current)
		}
	}
	return nil
}

func createConfigSeedParents(root, relDir string) error {
	if relDir == "." {
		return nil
	}
	current := root
	for _, part := range strings.Split(relDir, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		err := os.Mkdir(current, 0o755)
		if err == nil {
			continue
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s must not be a symlink", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", current)
		}
	}
	return nil
}

func writeConfigSeedFile(root string, file configSeedFile) error {
	rel := filepath.FromSlash(file.path)
	target := filepath.Join(root, rel)
	if err := createConfigSeedParents(root, filepath.Dir(rel)); err != nil {
		return fmt.Errorf("create parent for %s: %w", file.path, err)
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create %s: destination changed after preflight", file.path)
	}
	if err != nil {
		return fmt.Errorf("create %s: %w", file.path, err)
	}
	written, writeErr := output.Write(file.data)
	if writeErr == nil && written != len(file.data) {
		writeErr = io.ErrShortWrite
	}
	closeErr := output.Close()
	if writeErr == nil && closeErr == nil {
		return nil
	}
	removeErr := os.Remove(target)
	return errors.Join(
		wrapConfigSeedFileError("write", file.path, writeErr),
		wrapConfigSeedFileError("close", file.path, closeErr),
		wrapConfigSeedFileError("remove incomplete", file.path, removeErr),
	)
}

func wrapConfigSeedFileError(action, filePath string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s %s: %w", action, filePath, err)
}
