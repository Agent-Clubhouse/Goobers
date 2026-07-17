package instance

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// starterFS embeds a minimal, valid starter config repo (mirrors
// config-examples/) seeded into a freshly initialized instance's config/ dir.
//
//go:embed starter demo
var starterFS embed.FS

const (
	starterDir = "starter"
	demoDir    = "demo"
)

// InitResult reports what Init created vs. left alone.
type InitResult struct {
	Root    string
	Created []string
	Skipped []string
}

// Init scaffolds an instance root at root: instance.yaml, config/ (seeded
// with a starter example), runs/, scheduler/, workcopies/, and a
// telemetry.db placeholder (INST-010, ARCHITECTURE.md §6).
//
// Init is idempotent and non-destructive: any piece that already exists is
// left untouched and reported under Skipped, so a repeated `goobers init`
// never clobbers user edits (INST-008).
func Init(root string) (*InitResult, error) {
	return initWithConfig(root, starterDir, defaultConfig())
}

// InitDemo scaffolds a credential-free instance with one runnable,
// deterministic demo workflow.
func InitDemo(root string) (*InitResult, error) {
	return initWithConfig(root, demoDir, demoConfig())
}

func initWithConfig(root, configSource string, cfg *Config) (*InitResult, error) {
	l := NewLayout(root)
	res := &InitResult{Root: root}

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
		if err := copyConfig(l.ConfigDir(), configSource); err != nil {
			return nil, fmt.Errorf("seed %s: %w", ConfigDirName, err)
		}
		res.Created = append(res.Created, ConfigDirName)
	}

	for _, name := range []string{RunsDirName, SchedulerDirName, WorkcopiesDirName} {
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
				Provider: "github",
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

// copyConfig extracts one embedded config tree into dir.
func copyConfig(dir, source string) error {
	return fs.WalkDir(starterFS, source, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := starterFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}
