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
	"sort"
	"strings"
)

type config struct {
	Module       string         `json:"module"`
	Builds       []buildContext `json:"builds"`
	Forbidden    []boundary     `json:"forbidden"`
	Lanes        []lane         `json:"lanes"`
	ClosureRules []closureRule  `json:"closureRules,omitempty"`
}

type buildContext struct {
	Name   string   `json:"name"`
	GOOS   string   `json:"goos"`
	GOARCH string   `json:"goarch"`
	Tags   []string `json:"tags,omitempty"`
}

type boundary struct {
	Path      string `json:"path"`
	Rationale string `json:"rationale"`
}

type closureRule struct {
	Name          string         `json:"name"`
	Roots         []string       `json:"roots"`
	Forbidden     []boundary     `json:"forbidden"`
	ReviewedEdges []reviewedEdge `json:"reviewedEdges,omitempty"`
}

type reviewedEdge struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Rationale string `json:"rationale"`
}

type lane struct {
	Path              string   `json:"path"`
	Decision          string   `json:"decision"`
	Maintainer        string   `json:"maintainer,omitempty"`
	Rationale         string   `json:"rationale"`
	AllowedDirect     []string `json:"allowedDirect,omitempty"`
	AllowedTransitive []string `json:"allowedTransitive,omitempty"`
}

type listedPackage struct {
	ImportPath string         `json:"ImportPath"`
	ForTest    string         `json:"ForTest"`
	Imports    []string       `json:"Imports"`
	Error      *packageError  `json:"Error"`
	DepsErrors []packageError `json:"DepsErrors"`
}

type packageError struct {
	ImportStack []string `json:"ImportStack"`
	Err         string   `json:"Err"`
}

func loadConfig(path string) (config, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	var cfg config
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	if err := validateConfig(cfg); err != nil {
		return config{}, fmt.Errorf("config %q: %w", path, err)
	}
	return cfg, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func validateConfig(cfg config) error {
	if cfg.Module == "" {
		return errors.New("module is required")
	}
	if len(cfg.Lanes) == 0 {
		return errors.New("at least one lane is required")
	}
	if len(cfg.Builds) == 0 {
		return errors.New("at least one build context is required")
	}

	if err := validateBuilds(cfg.Builds); err != nil {
		return err
	}
	forbidden, err := validateForbidden(cfg.Module, cfg.Forbidden)
	if err != nil {
		return err
	}
	if err := validateLanes(cfg.Module, cfg.Lanes, forbidden); err != nil {
		return err
	}
	return validateClosureRules(cfg.Module, cfg.ClosureRules)
}

func validateBuilds(builds []buildContext) error {
	buildNames := make(map[string]struct{}, len(builds))
	for _, build := range builds {
		if build.Name == "" || build.GOOS == "" || build.GOARCH == "" {
			return errors.New("each build context requires name, goos, and goarch")
		}
		if _, duplicate := buildNames[build.Name]; duplicate {
			return fmt.Errorf("duplicate build context %q", build.Name)
		}
		buildNames[build.Name] = struct{}{}
	}
	return nil
}

func validateForbidden(module string, rules []boundary) (map[string]struct{}, error) {
	forbidden := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		if err := validateBoundary(module, rule, "forbidden boundary"); err != nil {
			return nil, err
		}
		if _, duplicate := forbidden[rule.Path]; duplicate {
			return nil, fmt.Errorf("duplicate forbidden boundary %q", rule.Path)
		}
		forbidden[rule.Path] = struct{}{}
	}
	return forbidden, nil
}

func validateLanes(module string, lanes []lane, forbidden map[string]struct{}) error {
	lanePaths := make(map[string]struct{}, len(lanes))
	for _, item := range lanes {
		if !isWithin(item.Path, module) || item.Path == module {
			return fmt.Errorf("lane path %q must be below module %q", item.Path, module)
		}
		if _, duplicate := lanePaths[item.Path]; duplicate {
			return fmt.Errorf("duplicate lane %q", item.Path)
		}
		lanePaths[item.Path] = struct{}{}
		if item.Rationale == "" {
			return fmt.Errorf("lane %q requires rationale", item.Path)
		}
		switch item.Decision {
		case "accepted", "pending":
			if item.Maintainer != "" {
				return fmt.Errorf("lane %q may name a maintainer only when declined", item.Path)
			}
		case "declined":
			if item.Maintainer == "" {
				return fmt.Errorf("declined lane %q requires a named maintainer", item.Path)
			}
		default:
			return fmt.Errorf("lane %q has invalid decision %q (want accepted, pending, or declined)", item.Path, item.Decision)
		}
		if err := validateAllowlist(module, item, forbidden); err != nil {
			return err
		}
	}
	return nil
}

func validateClosureRules(module string, rules []closureRule) error {
	closureNames := make(map[string]struct{}, len(rules))
	for _, rule := range rules {
		if strings.TrimSpace(rule.Name) == "" || len(rule.Roots) == 0 || len(rule.Forbidden) == 0 {
			return errors.New("each closure rule requires name, roots, and forbidden prefixes")
		}
		if _, duplicate := closureNames[rule.Name]; duplicate {
			return fmt.Errorf("duplicate closure rule %q", rule.Name)
		}
		if err := validateReviewedEdges(module, rule); err != nil {
			return err
		}

		closureNames[rule.Name] = struct{}{}
		roots := make(map[string]struct{}, len(rule.Roots))
		for _, root := range rule.Roots {
			if !isWithin(root, module) || root == module {
				return fmt.Errorf("closure rule %q root %q must be below module %q", rule.Name, root, module)
			}
			if _, duplicate := roots[root]; duplicate {
				return fmt.Errorf("closure rule %q repeats root %q", rule.Name, root)
			}
			roots[root] = struct{}{}
		}
		prefixes := make(map[string]struct{}, len(rule.Forbidden))
		for _, denied := range rule.Forbidden {
			if strings.TrimSpace(denied.Path) == "" || strings.TrimSpace(denied.Rationale) == "" {
				return fmt.Errorf("closure rule %q forbidden prefixes require path and rationale", rule.Name)
			}
			if _, duplicate := prefixes[denied.Path]; duplicate {
				return fmt.Errorf("closure rule %q repeats forbidden prefix %q", rule.Name, denied.Path)
			}
			prefixes[denied.Path] = struct{}{}
		}
	}
	return nil
}

func validateBoundary(module string, rule boundary, kind string) error {
	if !isWithin(rule.Path, module) || rule.Path == module {
		return fmt.Errorf("%s %q must be below module %q", kind, rule.Path, module)
	}
	if rule.Rationale == "" {
		return fmt.Errorf("%s %q requires rationale", kind, rule.Path)
	}
	return nil
}

func validateAllowlist(module string, item lane, forbidden map[string]struct{}) error {
	transitive := make(map[string]struct{}, len(item.AllowedTransitive))
	for _, path := range item.AllowedTransitive {
		if err := validateAllowedPath(module, item.Path, path, forbidden); err != nil {
			return err
		}
		if _, duplicate := transitive[path]; duplicate {
			return fmt.Errorf("lane %q repeats transitive allowance %q", item.Path, path)
		}
		transitive[path] = struct{}{}
	}
	direct := make(map[string]struct{}, len(item.AllowedDirect))
	for _, path := range item.AllowedDirect {
		if err := validateAllowedPath(module, item.Path, path, forbidden); err != nil {
			return err
		}
		if _, duplicate := direct[path]; duplicate {
			return fmt.Errorf("lane %q repeats direct allowance %q", item.Path, path)
		}
		direct[path] = struct{}{}
		if !matchesAny(path, item.AllowedTransitive) {
			return fmt.Errorf("lane %q direct allowance %q must also be in its transitive closure", item.Path, path)
		}
	}
	return nil
}

func validateAllowedPath(module, lanePath, path string, forbidden map[string]struct{}) error {
	if !isWithin(path, module) || path == module {
		return fmt.Errorf("lane %q allowance %q must be below module %q", lanePath, path, module)
	}
	for denied := range forbidden {
		if pathsOverlap(path, denied) {
			return fmt.Errorf("lane %q allowance %q overlaps forbidden boundary %q", lanePath, path, denied)
		}
	}
	return nil
}

func check(ctx context.Context, root string, cfg config) (string, error) {
	var accepted, pending, declined int
	for _, item := range cfg.Lanes {
		switch item.Decision {
		case "accepted":
			accepted++
		case "pending":
			pending++
		case "declined":
			declined++
		}
	}
	if accepted == 0 {
		if err := checkClosures(ctx, root, cfg); err != nil {
			return "", err
		}
		if pending > 0 {
			return fmt.Sprintf("no accepted pilot namespaces; %d pending lane(s) remain preregistered, synthetic checks still apply", pending), nil
		}
		return fmt.Sprintf("real-package checking is inapplicable: all %d pilot lane(s) were declined by named maintainers", declined), nil
	}

	for _, build := range cfg.Builds {
		for _, item := range cfg.Lanes {
			if item.Decision != "accepted" {
				continue
			}
			graph, err := loadGraph(ctx, root, build, item.Path)
			if err != nil {
				return "", fmt.Errorf("%s (%s): %w", item.Path, build.Name, err)
			}
			if err := checkLane(cfg, item, graph); err != nil {
				return "", fmt.Errorf("%s (%s): %w", item.Path, build.Name, err)
			}
		}
	}
	if err := checkClosures(ctx, root, cfg); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d accepted pilot lane(s) and %d closure rule(s) satisfy reviewed boundaries in %d build context(s)", accepted, len(cfg.ClosureRules), len(cfg.Builds)), nil
}

func checkClosures(ctx context.Context, root string, cfg config) error {
	for _, build := range cfg.Builds {
		for _, rule := range cfg.ClosureRules {
			for _, importPath := range rule.Roots {
				graph, err := loadPackageGraph(ctx, root, build, importPath, false, false)
				if err != nil {
					return fmt.Errorf("closure %s root %s (%s): %w", rule.Name, importPath, build.Name, err)
				}
				if err := checkClosure(rule, importPath, graph); err != nil {
					return fmt.Errorf("closure %s root %s (%s): %w", rule.Name, importPath, build.Name, err)
				}
			}
		}
	}
	return nil
}

func loadGraph(ctx context.Context, root string, build buildContext, importPath string) (map[string]listedPackage, error) {
	return loadPackageGraph(ctx, root, build, importPath, true, true)
}

func loadPackageGraph(ctx context.Context, root string, build buildContext, importPath string, includeTests, recursive bool) (map[string]listedPackage, error) {
	args := []string{"list", "-json", "-deps", "-e"}
	if includeTests {
		args = append(args, "-test")
	}
	if len(build.Tags) > 0 {
		args = append(args, "-tags="+strings.Join(build.Tags, ","))
	}
	if recursive {
		importPath += "/..."
	}
	args = append(args, importPath)
	command := exec.CommandContext(ctx, "go", args...)
	command.Dir = root
	command.Env = append(os.Environ(), "GOOS="+build.GOOS, "GOARCH="+build.GOARCH, "CGO_ENABLED=0")
	var output, diagnostic bytes.Buffer
	command.Stdout = &output
	command.Stderr = &diagnostic
	err := command.Run()
	if err != nil {
		return nil, fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(diagnostic.String()))
	}

	graph := make(map[string]listedPackage)
	decoder := json.NewDecoder(&output)
	for {
		var pkg listedPackage
		err := decoder.Decode(&pkg)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		graph[pkg.ImportPath] = pkg
	}
	if message := strings.TrimSpace(diagnostic.String()); message != "" {
		return nil, fmt.Errorf("package discovery failed for namespace %s: %s", importPath, message)
	}
	if len(graph) == 0 {
		return nil, fmt.Errorf("package discovery failed for namespace %s: go list returned no packages", importPath)
	}
	for _, pkg := range graph {
		if pkg.Error != nil {
			return nil, formatPackageError(pkg.ImportPath, *pkg.Error)
		}
		if len(pkg.DepsErrors) > 0 {
			return nil, formatPackageError(pkg.ImportPath, pkg.DepsErrors[0])
		}
	}
	return graph, nil
}

func validateReviewedEdges(module string, rule closureRule) error {
	seen := make(map[string]bool)
	for _, edge := range rule.ReviewedEdges {
		key := edge.From + " -> " + edge.To
		if !isWithin(edge.From, module) || edge.To == "" || edge.Rationale == "" || seen[key] {
			return fmt.Errorf("invalid reviewed closure edge %s", key)
		}
		denied := false
		for _, boundary := range rule.Forbidden {
			denied = denied || isWithin(edge.To, boundary.Path)
		}
		if !denied {
			return fmt.Errorf("reviewed closure edge %s does not target a forbidden dependency", key)
		}
		seen[key] = true
	}
	return nil
}

func checkClosure(rule closureRule, root string, graph map[string]listedPackage) error {
	if _, ok := graph[root]; !ok {
		return fmt.Errorf("root is missing from the Go package graph")
	}
	seenEdges := make(map[string]bool)
	queue := []graphStep{{path: root, chain: []string{root}}}
	visited := make(map[string]bool)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if visited[current.path] {
			continue
		}
		visited[current.path] = true
		pkg := graph[current.path]
		imports := append([]string(nil), pkg.Imports...)
		sort.Strings(imports)
		for _, imported := range imports {
			chain := append(append([]string(nil), current.chain...), imported)
			for _, denied := range rule.Forbidden {
				if isWithin(imported, denied.Path) {
					if reviewedClosureEdge(rule, current.path, imported) {
						seenEdges[current.path+" -> "+imported] = true
						continue
					}
					return fmt.Errorf("forbidden cloud dependency %s (%s); chain: %s", imported, denied.Rationale, strings.Join(chain, " -> "))
				}
			}
			if _, exists := graph[imported]; exists {
				queue = append(queue, graphStep{path: imported, chain: chain})
			}
		}
	}
	for _, edge := range rule.ReviewedEdges {
		if !seenEdges[edge.From+" -> "+edge.To] {
			return fmt.Errorf("obsolete reviewed closure edge %s -> %s; remove its exception", edge.From, edge.To)
		}
	}

	return nil
}

func reviewedClosureEdge(rule closureRule, from, to string) bool {
	for _, edge := range rule.ReviewedEdges {
		if edge.From == from && edge.To == to {
			return true
		}
	}
	return false
}

func formatPackageError(importPath string, problem packageError) error {
	chain := problem.ImportStack
	if len(chain) == 0 {
		chain = []string{normalizeImportPath(importPath)}
	}
	return fmt.Errorf("package discovery failed along %s: %s", strings.Join(chain, " -> "), problem.Err)
}

func checkLane(cfg config, item lane, graph map[string]listedPackage) error {
	roots := laneRoots(item.Path, graph)
	if len(roots) == 0 {
		return fmt.Errorf("accepted namespace is missing from the Go package graph: %s", item.Path)
	}
	for _, root := range roots {
		if err := walkImports(cfg, item, root, graph); err != nil {
			return err
		}
	}
	return nil
}

type graphStep struct {
	path  string
	chain []string
	depth int
}

func walkImports(cfg config, item lane, root string, graph map[string]listedPackage) error {
	queue := []graphStep{{path: root, chain: []string{normalizeImportPath(root)}}}
	visited := make(map[string]bool)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if visited[current.path] {
			continue
		}
		visited[current.path] = true
		pkg, ok := graph[current.path]
		if !ok {
			return fmt.Errorf("package graph is missing metadata for %s along %s", current.path, strings.Join(current.chain, " -> "))
		}
		imports := append([]string(nil), pkg.Imports...)
		sort.Strings(imports)
		for _, imported := range imports {
			display := normalizeImportPath(imported)
			chain := append(append([]string(nil), current.chain...), display)
			if err := validateImport(cfg, item, display, current.depth == 0, chain); err != nil {
				return err
			}
			if _, exists := graph[imported]; exists {
				queue = append(queue, graphStep{path: imported, chain: chain, depth: current.depth + 1})
			}
		}
	}
	return nil
}

func validateImport(cfg config, item lane, imported string, direct bool, chain []string) error {
	if !isWithin(imported, cfg.Module) {
		return nil
	}
	for _, rule := range cfg.Forbidden {
		if isWithin(imported, rule.Path) {
			return fmt.Errorf("forbidden import %s (%s); chain: %s", imported, rule.Rationale, strings.Join(chain, " -> "))
		}
	}
	for _, other := range cfg.Lanes {
		if other.Path != item.Path && isWithin(imported, other.Path) {
			return fmt.Errorf("pilot-to-pilot import %s is not reviewed; chain: %s", imported, strings.Join(chain, " -> "))
		}
	}
	if isWithin(imported, item.Path) {
		return nil
	}
	allowed := item.AllowedTransitive
	kind := "transitive"
	if direct {
		allowed = item.AllowedDirect
		kind = "direct"
	}
	if !matchesAny(imported, allowed) {
		return fmt.Errorf("unreviewed %s import %s; chain: %s", kind, imported, strings.Join(chain, " -> "))
	}
	return nil
}

func laneRoots(path string, graph map[string]listedPackage) []string {
	var roots []string
	for importPath := range graph {
		normalized := normalizeImportPath(importPath)
		if isWithin(normalized, path) || isWithin(strings.TrimSuffix(normalized, "_test"), path) {
			roots = append(roots, importPath)
		}
	}
	sort.Strings(roots)
	return roots
}

func normalizeImportPath(path string) string {
	if before, _, found := strings.Cut(path, " ["); found {
		return before
	}
	return path
}

func matchesAny(path string, allowed []string) bool {
	for _, prefix := range allowed {
		if isWithin(path, prefix) {
			return true
		}
	}
	return false
}

func isWithin(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func pathsOverlap(left, right string) bool {
	return isWithin(left, right) || isWithin(right, left)
}
