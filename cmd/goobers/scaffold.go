package main

import (
	"bytes"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/instance"
)

//go:embed templates/scaffold/*.tmpl
var scaffoldTemplateFS embed.FS

var scaffoldNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type scaffoldTarget struct {
	instanceRoot string
	gaggleDir    string
	gaggle       string
}

type scaffoldTemplateData struct {
	Name   string
	Gaggle string
	Goober string
}

type scaffoldFile struct {
	path     string
	template string
}

func runScaffold(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		scaffoldUsage(stderr)
		return 2
	}
	switch args[0] {
	case "goober", "workflow":
		return runScaffoldKind(args[0], args[1:], stdout, stderr)
	default:
		pf(stderr, "goobers scaffold: unknown kind %q\n\n", args[0])
		scaffoldUsage(stderr)
		return 2
	}
}

func scaffoldUsage(w io.Writer) {
	pf(w, "Usage: goobers scaffold goober [--force] <name> [path]\n"+
		"       goobers scaffold workflow [--force] <name> [path]\n\n"+
		"Generate a valid goober or workflow in the current gaggle. path may be\n"+
		"an instance root or a gaggle directory and defaults to \".\". Existing\n"+
		"files are never replaced unless --force is set.\n")
}

func runScaffoldKind(kind string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("scaffold "+kind, flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "replace generated files that already exist")
	fs.Usage = func() { scaffoldUsage(stderr) }
	normalized := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--force" {
			normalized = append([]string{arg}, normalized...)
			continue
		}
		normalized = append(normalized, arg)
	}
	if err := fs.Parse(normalized); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)
	if !scaffoldNamePattern.MatchString(name) {
		pf(stderr, "error: invalid name %q (use lowercase letters, digits, and interior hyphens)\n", name)
		return 2
	}
	path := "."
	if fs.NArg() == 2 {
		path = fs.Arg(1)
	}

	target, err := resolveScaffoldTarget(path)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	data := scaffoldTemplateData{Name: name, Gaggle: target.gaggle}
	files := scaffoldFiles(kind, target.gaggleDir, name)
	next := fmt.Sprintf("goobers validate %s", target.instanceRoot)
	if kind == "workflow" {
		data.Goober, err = scaffoldGoober(target.gaggleDir, target.gaggle)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		next = fmt.Sprintf("goobers run %s %s", name, target.instanceRoot)
	}

	rendered := make([][]byte, len(files))
	for i, file := range files {
		rendered[i], err = renderScaffoldTemplate(file.template, data)
		if err != nil {
			pf(stderr, "error: render %s: %v\n", file.template, err)
			return 2
		}
	}
	if err := writeScaffoldFiles(target.instanceRoot, target.gaggleDir, files, rendered, *force); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	for _, file := range files {
		pf(stdout, "created %s\n", file.path)
	}
	pf(stdout, "next: %s\n", next)
	return 0
}

func resolveScaffoldTarget(path string) (*scaffoldTarget, error) {
	start, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path %s: %w", path, err)
	}
	info, err := os.Stat(start)
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", start, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", start)
	}

	root, err := findInstanceRoot(start)
	if err != nil {
		return nil, err
	}
	layout := instance.NewLayout(root)
	manifest, err := readScaffoldManifest(layout.ConfigDir())
	if err != nil {
		return nil, err
	}

	var gaggle, gaggleDir string
	if start == root {
		if len(manifest.Spec.Gaggles) != 1 {
			return nil, fmt.Errorf("instance has %d active gaggles; run the command from the intended gaggle directory", len(manifest.Spec.Gaggles))
		}
		gaggle = manifest.Spec.Gaggles[0]
		gaggleDir = filepath.Join(layout.ConfigDir(), "gaggles", gaggle)
	} else {
		configGaggles := filepath.Join(layout.ConfigDir(), "gaggles")
		rel, relErr := filepath.Rel(configGaggles, start)
		if relErr != nil || rel == "." || filepath.Dir(rel) != "." {
			return nil, fmt.Errorf("%s is not an instance root or gaggle directory", start)
		}
		gaggle = filepath.Base(start)
		gaggleDir = start
		active := false
		for _, name := range manifest.Spec.Gaggles {
			if name == gaggle {
				active = true
				break
			}
		}
		if !active {
			return nil, fmt.Errorf("gaggle %q is not active in the instance manifest", gaggle)
		}
	}
	if info, err := os.Stat(gaggleDir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("gaggle directory %s not found", gaggleDir)
	}
	var definition apiv1.Gaggle
	gaggleFile := filepath.Join(gaggleDir, "gaggle.yaml")
	if err := readScaffoldYAML(gaggleFile, &definition); err != nil {
		return nil, err
	}
	if definition.Name != gaggle {
		return nil, fmt.Errorf("%s defines gaggle %q, want %q", gaggleFile, definition.Name, gaggle)
	}
	return &scaffoldTarget{instanceRoot: root, gaggleDir: gaggleDir, gaggle: gaggle}, nil
}

func findInstanceRoot(start string) (string, error) {
	for dir := start; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(instance.NewLayout(dir).ConfigFile()); err == nil {
			return dir, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("inspect %s: %w", instance.NewLayout(dir).ConfigFile(), err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no instance root found from %s (run `goobers init` first)", start)
		}
	}
}

func readScaffoldManifest(configDir string) (*apiv1.Manifest, error) {
	path := filepath.Join(configDir, "manifest.yaml")
	var manifest apiv1.Manifest
	if err := readScaffoldYAML(path, &manifest); err != nil {
		return nil, err
	}
	if len(manifest.Spec.Gaggles) == 0 {
		return nil, fmt.Errorf("%s has no active gaggles", path)
	}
	return &manifest, nil
}

func readScaffoldYAML(path string, into interface{}) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func scaffoldGoober(gaggleDir, gaggle string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(gaggleDir, "goobers"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("gaggle %q has no goobers; run `goobers scaffold goober <name>` first", gaggle)
		}
		return "", fmt.Errorf("list goobers for gaggle %q: %w", gaggle, err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		var goober apiv1.Goober
		path := filepath.Join(gaggleDir, "goobers", entry.Name(), "goober.yaml")
		if err := readScaffoldYAML(path, &goober); err != nil {
			return "", err
		}
		if goober.Spec.Gaggle != gaggle {
			continue
		}
		for _, grant := range goober.Spec.Capabilities {
			if grant == string(capability.AgentModel) {
				names = append(names, goober.Name)
				break
			}
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", fmt.Errorf("gaggle %q has no goober with agent:model; run `goobers scaffold goober <name>` first", gaggle)
	}
	return names[0], nil
}

func scaffoldFiles(kind, gaggleDir, name string) []scaffoldFile {
	if kind == "goober" {
		dir := filepath.Join(gaggleDir, "goobers", name)
		return []scaffoldFile{
			{path: filepath.Join(dir, "goober.yaml"), template: "templates/scaffold/goober.yaml.tmpl"},
			{path: filepath.Join(dir, "instructions.md"), template: "templates/scaffold/instructions.md.tmpl"},
		}
	}
	return []scaffoldFile{{
		path:     filepath.Join(gaggleDir, "workflows", name+".yaml"),
		template: "templates/scaffold/workflow.yaml.tmpl",
	}}
}

func renderScaffoldTemplate(name string, data scaffoldTemplateData) ([]byte, error) {
	raw, err := scaffoldTemplateFS.ReadFile(name)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New(filepath.Base(name)).Parse(string(raw))
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeScaffoldFiles(instanceRoot, gaggleDir string, files []scaffoldFile, contents [][]byte, force bool) error {
	if err := validateScaffoldDestinations(instanceRoot, gaggleDir, files); err != nil {
		return err
	}
	for _, file := range files {
		_, err := os.Lstat(file.path)
		switch {
		case err == nil && !force:
			return fmt.Errorf("refusing to overwrite %s (use --force)", file.path)
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("inspect %s: %w", file.path, err)
		}
	}
	for i, file := range files {
		if err := os.MkdirAll(filepath.Dir(file.path), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(file.path), err)
		}
		if force {
			if err := os.Remove(file.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("replace %s: %w", file.path, err)
			}
		}
		f, err := os.OpenFile(file.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("create %s: %w", file.path, err)
		}
		if _, err := f.Write(contents[i]); err != nil {
			_ = f.Close()
			return fmt.Errorf("write %s: %w", file.path, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("close %s: %w", file.path, err)
		}
	}
	return nil
}

func validateScaffoldDestinations(instanceRoot, gaggleDir string, files []scaffoldFile) error {
	resolvedRoot, err := filepath.EvalSymlinks(instanceRoot)
	if err != nil {
		return fmt.Errorf("resolve instance root %s: %w", instanceRoot, err)
	}
	resolvedGaggle, err := filepath.EvalSymlinks(gaggleDir)
	if err != nil {
		return fmt.Errorf("resolve gaggle directory %s: %w", gaggleDir, err)
	}
	if !pathWithin(resolvedRoot, resolvedGaggle) {
		return fmt.Errorf("gaggle directory %s resolves outside instance root %s", gaggleDir, instanceRoot)
	}

	for _, file := range files {
		parent := filepath.Dir(file.path)
		rel, err := filepath.Rel(gaggleDir, parent)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return fmt.Errorf("scaffold destination %s is outside gaggle directory %s", file.path, gaggleDir)
		}
		current := gaggleDir
		for _, part := range splitPath(rel) {
			current = filepath.Join(current, part)
			info, err := os.Lstat(current)
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			if err != nil {
				return fmt.Errorf("inspect scaffold directory %s: %w", current, err)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing scaffold destination through symlinked directory %s", current)
			}
			if !info.IsDir() {
				return fmt.Errorf("scaffold path component %s is not a directory", current)
			}
		}
	}
	return nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(rel)
}

func splitPath(path string) []string {
	if path == "." {
		return nil
	}
	var parts []string
	for path != "." {
		dir, base := filepath.Split(path)
		parts = append([]string{base}, parts...)
		path = filepath.Clean(dir)
	}
	return parts
}
