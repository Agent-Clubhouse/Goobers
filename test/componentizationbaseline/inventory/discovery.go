package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

type commandResult struct {
	stdout []byte
	stderr []byte
}

type commandRunner interface {
	run(context.Context, string, []string, []string) (commandResult, error)
}

type execRunner struct{}

func (execRunner) run(ctx context.Context, name string, args, env []string) (commandResult, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return commandResult{stdout: stdout.Bytes(), stderr: stderr.Bytes()}, err
}

type moduleMetadata struct {
	Path string
	Dir  string
}

type goEnv struct {
	GOOS       string
	GOARCH     string
	CGOEnabled string `json:"CGO_ENABLED"`
	GoVersion  string `json:"GOVERSION"`
}

type goPackage struct {
	ImportPath     string
	Name           string
	Dir            string
	GoFiles        []string
	CgoFiles       []string
	TestGoFiles    []string
	XTestGoFiles   []string
	IgnoredGoFiles []string
	Imports        []string
	TestImports    []string
	XTestImports   []string
	ForTest        string
	Export         string
	Module         *struct {
		Path string
		Dir  string
		Main bool
	}
	Error *struct {
		Err string
	}
	DepsErrors []struct {
		ImportStack []string
		Pos         string
		Err         string
	}
}

type discovery struct {
	runner commandRunner
	goos   string
	goarch string
	tags   []string
}

func (d discovery) collect(ctx context.Context, target string) (inventory, error) {
	env := d.environment()
	moduleResult, err := d.invoke(ctx, "go", []string{"list", "-m", "-json"}, env)
	if err != nil {
		return inventory{}, err
	}
	var module moduleMetadata
	if err := decodeOne(moduleResult.stdout, &module); err != nil {
		return inventory{}, fmt.Errorf("decode `go list -m -json`: %w", err)
	}
	if module.Path == "" || module.Dir == "" {
		return inventory{}, errors.New("decode `go list -m -json`: missing module Path or Dir")
	}

	envResult, err := d.invoke(ctx, "go", []string{"env", "-json", "GOOS", "GOARCH", "CGO_ENABLED", "GOVERSION"}, env)
	if err != nil {
		return inventory{}, err
	}
	var build goEnv
	if err := decodeOne(envResult.stdout, &build); err != nil {
		return inventory{}, fmt.Errorf("decode `go env -json`: %w", err)
	}
	if build.GOOS == "" || build.GOARCH == "" || build.CGOEnabled == "" || build.GoVersion == "" {
		return inventory{}, errors.New("decode `go env -json`: missing GOOS, GOARCH, CGO_ENABLED, or GOVERSION")
	}

	prodArgs := append([]string{"list", "-deps", "-export", "-json"}, d.tagArgs()...)
	prodArgs = append(prodArgs, target)
	prodResult, err := d.invoke(ctx, "go", prodArgs, env)
	if err != nil {
		return inventory{}, err
	}
	prod, err := decodePackages(prodResult.stdout, "production go list")
	if err != nil {
		return inventory{}, err
	}

	testRoots := localClosure(module.Path, prod)
	var test []goPackage
	for {
		testArgs := append([]string{"list", "-deps", "-test", "-export", "-json"}, d.tagArgs()...)
		testArgs = append(testArgs, testRoots...)
		testResult, err := d.invoke(ctx, "go", testArgs, env)
		if err != nil {
			return inventory{}, err
		}
		test, err = decodePackages(testResult.stdout, "test go list")
		if err != nil {
			return inventory{}, err
		}
		nextRoots := localClosure(module.Path, test)
		if slices.Equal(testRoots, nextRoots) {
			break
		}
		testRoots = nextRoots
	}

	commitResult, err := d.invoke(ctx, "git", []string{"-C", module.Dir, "rev-parse", "HEAD"}, nil)
	if err != nil {
		return inventory{}, err
	}
	commit := strings.TrimSpace(string(commitResult.stdout))
	if commit == "" {
		return inventory{}, errors.New("decode `git rev-parse HEAD`: empty commit")
	}

	return buildInventory(module, build, commit, d.tags, target, prod, test)
}

func (d discovery) invoke(ctx context.Context, name string, args, env []string) (commandResult, error) {
	result, err := d.runner.run(ctx, name, args, env)
	if err == nil {
		return result, nil
	}
	detail := strings.TrimSpace(string(result.stderr))
	if detail == "" {
		detail = strings.TrimSpace(string(result.stdout))
	}
	if detail == "" {
		detail = err.Error()
	}
	return commandResult{}, fmt.Errorf("%s %s failed: %s", name, strings.Join(args, " "), detail)
}

func (d discovery) environment() []string {
	var env []string
	if d.goos != "" {
		env = append(env, "GOOS="+d.goos)
	}
	if d.goarch != "" {
		env = append(env, "GOARCH="+d.goarch)
	}
	return env
}

func (d discovery) tagArgs() []string {
	if len(d.tags) == 0 {
		return nil
	}
	return []string{"-tags", strings.Join(d.tags, ",")}
}

func decodeOne(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected additional JSON value")
		}
		return err
	}
	return nil
}

func decodePackages(data []byte, label string) ([]goPackage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var packages []goPackage
	for {
		var pkg goPackage
		err := decoder.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode %s package %d: %w", label, len(packages)+1, err)
		}
		if pkg.Error != nil {
			if pkg.Error.Err == "" {
				return nil, fmt.Errorf("decode %s package %d: package Error is missing Err", label, len(packages)+1)
			}
			return nil, fmt.Errorf("%s package %q: %s", label, pkg.ImportPath, pkg.Error.Err)
		}
		if len(pkg.DepsErrors) > 0 {
			e := pkg.DepsErrors[0]
			if e.Err == "" {
				return nil, fmt.Errorf("decode %s package %d: dependency error is missing Err", label, len(packages)+1)
			}
			return nil, fmt.Errorf("%s package %q dependency error at %s via %s: %s",
				label, pkg.ImportPath, e.Pos, strings.Join(e.ImportStack, " -> "), e.Err)
		}
		if pkg.ImportPath == "" || pkg.Name == "" || pkg.Dir == "" {
			return nil, fmt.Errorf("decode %s package %d: missing ImportPath, Name, or Dir", label, len(packages)+1)
		}
		packages = append(packages, pkg)
	}
	if len(packages) == 0 {
		return nil, fmt.Errorf("decode %s: no packages", label)
	}
	return packages, nil
}

func repositoryPath(root, path string) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside module root %s", path, root)
	}
	if rel == "." {
		return ".", nil
	}
	return filepath.ToSlash(rel), nil
}

func sortedUnique(values ...[]string) []string {
	set := make(map[string]struct{})
	for _, list := range values {
		for _, value := range list {
			if value != "" {
				set[value] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func difference(left, right []string) []string {
	rightSet := make(map[string]struct{}, len(right))
	for _, value := range right {
		rightSet[value] = struct{}{}
	}
	var out []string
	for _, value := range left {
		if _, ok := rightSet[value]; !ok {
			out = append(out, value)
		}
	}
	return out
}
