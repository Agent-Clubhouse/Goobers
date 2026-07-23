package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/providers"
)

const maxStructuralCollisionHunks = 3

type rebaseConflictLocation struct {
	Path  string `json:"path"`
	Scope string `json:"scope,omitempty"`
}

type patchHunk struct {
	Scope string
	Text  string
}

type structuralCollision struct {
	SiblingNumber int
	Path          string
	Function      string
	CurrentHunk   string
	SiblingHunk   string
}

type goFunctionSnapshot struct {
	Lines []string
}

var goFunctionScopePattern = regexp.MustCompile(`^func\s+(?:\(([^)]*)\)\s*)?([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
var combinedHunkTheirsLinePattern = regexp.MustCompile(`^@@@ -[0-9]+(?:,[0-9]+)? -([0-9]+)(?:,[0-9]+)? \+[0-9]+(?:,[0-9]+)? @@@`)

func currentRebaseConflictLocations(dir string) ([]rebaseConflictLocation, error) {
	cmd := exec.Command("git", "diff", "--name-only", "--diff-filter=U", "-z")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, gitOutputError("git diff --name-only --diff-filter=U", err)
	}

	var locations []rebaseConflictLocation
	seen := make(map[string]bool)
	for _, rawPath := range strings.Split(string(out), "\x00") {
		if rawPath == "" {
			continue
		}
		diff := exec.Command("git", "diff", "--cc", "--unified=0", "--no-color", "--", rawPath)
		diff.Dir = dir
		patch, err := diff.Output()
		if err != nil {
			return nil, gitOutputError("git diff --cc --unified=0 -- "+rawPath, err)
		}
		foundScope := false
		for _, line := range strings.Split(string(patch), "\n") {
			scope, ok := unifiedHunkScope(line)
			if !ok {
				continue
			}
			if scope == "" && filepath.Ext(rawPath) == ".go" {
				theirsLine, ok := combinedHunkTheirsLine(line)
				if ok {
					scope = conflictGoFunctionScope(dir, rawPath, theirsLine)
				}
			}
			foundScope = true
			key := rawPath + "\x00" + scope
			if !seen[key] {
				locations = append(locations, rebaseConflictLocation{Path: rawPath, Scope: scope})
				seen[key] = true
			}
		}
		if !foundScope && !seen[rawPath+"\x00"] {
			locations = append(locations, rebaseConflictLocation{Path: rawPath})
			seen[rawPath+"\x00"] = true
		}
	}
	sort.Slice(locations, func(i, j int) bool {
		if locations[i].Path != locations[j].Path {
			return locations[i].Path < locations[j].Path
		}
		return locations[i].Scope < locations[j].Scope
	})
	return locations, nil
}

func combinedHunkTheirsLine(header string) (int, bool) {
	match := combinedHunkTheirsLinePattern.FindStringSubmatch(header)
	if match == nil {
		return 0, false
	}
	line, err := strconv.Atoi(match[1])
	return line, err == nil
}

func conflictGoFunctionScope(dir, path string, line int) string {
	show := exec.Command("git", "show", ":3:"+path)
	show.Dir = dir
	source, err := show.Output()
	if err != nil {
		return ""
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, source, parser.SkipObjectResolution)
	if err != nil {
		return ""
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		start := fset.Position(function.Pos()).Line
		end := fset.Position(function.End()).Line
		if line < start || line > end {
			continue
		}
		scope := "func "
		if function.Recv != nil && len(function.Recv.List) > 0 {
			receiver := function.Recv.List[0].Type
			from := fset.Position(receiver.Pos()).Offset
			to := fset.Position(receiver.End()).Offset
			scope += "(" + strings.TrimSpace(string(source[from:to])) + ") "
		}
		return scope + function.Name.Name + "("
	}
	return ""
}

func gitOutputError(operation string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("%s: %w: %s", operation, err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func unifiedHunkScope(line string) (string, bool) {
	atCount := 0
	for atCount < len(line) && line[atCount] == '@' {
		atCount++
	}
	if atCount < 2 {
		return "", false
	}
	marker := strings.Repeat("@", atCount)
	rest := line[atCount:]
	end := strings.Index(rest, marker)
	if end < 0 {
		return "", false
	}
	return strings.TrimSpace(rest[end+len(marker):]), true
}

func parsePatchHunks(patch string) []patchHunk {
	var hunks []patchHunk
	var current *patchHunk
	for _, line := range strings.Split(patch, "\n") {
		scope, header := unifiedHunkScope(line)
		if header {
			if current != nil {
				current.Text = strings.TrimSuffix(current.Text, "\n")
				hunks = append(hunks, *current)
			}
			current = &patchHunk{Scope: scope, Text: line + "\n"}
			continue
		}
		if current != nil {
			current.Text += line + "\n"
		}
	}
	if current != nil {
		current.Text = strings.TrimSuffix(current.Text, "\n")
		hunks = append(hunks, *current)
	}
	return hunks
}

func functionScopeKey(scope string) string {
	match := goFunctionScopePattern.FindStringSubmatch(strings.TrimSpace(scope))
	if match == nil {
		return ""
	}
	name := match[2]
	if match[1] == "" {
		return name
	}
	fields := strings.Fields(match[1])
	receiver := fields[len(fields)-1]
	receiver = strings.TrimLeft(receiver, "*[]")
	if receiver == "" {
		return name
	}
	return receiver + "." + name
}

func hunkTouchesFunction(hunk patchHunk, function string) bool {
	if functionScopeKey(hunk.Scope) == function {
		return true
	}
	for _, line := range strings.Split(hunk.Text, "\n") {
		if len(line) < 2 || (line[0] != '+' && line[0] != '-') {
			continue
		}
		if functionScopeKey(strings.TrimSpace(line[1:])) == function {
			return true
		}
	}
	return false
}

func hunkStructurallyRewritten(hunk patchHunk, function string) bool {
	var additions, deletions int
	var removedDeclaration, addedDeclaration string
	for _, line := range strings.Split(hunk.Text, "\n") {
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") || line == "" {
			continue
		}
		switch line[0] {
		case '+':
			additions++
			if functionScopeKey(strings.TrimSpace(line[1:])) == function {
				addedDeclaration = strings.TrimSpace(line[1:])
			}
		case '-':
			deletions++
			if functionScopeKey(strings.TrimSpace(line[1:])) == function {
				removedDeclaration = strings.TrimSpace(line[1:])
			}
		}
	}
	if removedDeclaration != "" && removedDeclaration != addedDeclaration {
		return true
	}
	changed := additions + deletions
	return (deletions >= 6 && additions >= 1 && changed >= 10) ||
		(deletions >= 4 && additions >= 4 && changed >= 12)
}

func hunksStructurallyRewritten(hunks []patchHunk, function string) bool {
	var text strings.Builder
	for _, hunk := range hunks {
		text.WriteString(hunk.Text)
		text.WriteString("\n")
	}
	return hunkStructurallyRewritten(patchHunk{Text: text.String()}, function)
}

func matchStructuralCollisions(
	conflicts []rebaseConflictLocation,
	currentFiles []providers.ChangedFile,
	siblingNumber int,
	siblingFiles []providers.ChangedFile,
) []structuralCollision {
	currentByPath := make(map[string][]patchHunk)
	for _, file := range currentFiles {
		currentByPath[file.Path] = parsePatchHunks(file.Patch)
		if file.PreviousPath != "" {
			currentByPath[file.PreviousPath] = append(currentByPath[file.PreviousPath], parsePatchHunks(file.Patch)...)
		}
	}
	siblingByPath := make(map[string][]patchHunk)
	for _, file := range siblingFiles {
		siblingByPath[file.Path] = parsePatchHunks(file.Patch)
		if file.PreviousPath != "" {
			siblingByPath[file.PreviousPath] = append(siblingByPath[file.PreviousPath], parsePatchHunks(file.Patch)...)
		}
	}

	var collisions []structuralCollision
	seen := make(map[string]bool)
	for _, conflict := range conflicts {
		function := functionScopeKey(conflict.Scope)
		if function == "" {
			continue
		}
		for _, currentHunk := range currentByPath[conflict.Path] {
			if !hunkTouchesFunction(currentHunk, function) {
				continue
			}
			key := strconv.Itoa(siblingNumber) + "\x00" + conflict.Path + "\x00" + function
			var relevantSiblingHunks []patchHunk
			for _, siblingHunk := range siblingByPath[conflict.Path] {
				if hunkTouchesFunction(siblingHunk, function) {
					relevantSiblingHunks = append(relevantSiblingHunks, siblingHunk)
				}
			}
			if !seen[key] && hunksStructurallyRewritten(relevantSiblingHunks, function) {
				seen[key] = true
				var hunkTexts []string
				for _, hunk := range relevantSiblingHunks {
					hunkTexts = append(hunkTexts, hunk.Text)
				}
				collisions = append(collisions, structuralCollision{
					SiblingNumber: siblingNumber,
					Path:          conflict.Path,
					Function:      strings.TrimSpace(conflict.Scope),
					CurrentHunk:   currentHunk.Text,
					SiblingHunk:   strings.Join(hunkTexts, "\n\n"),
				})
			}
			if seen[key] {
				continue
			}
			for _, siblingFile := range siblingFiles {
				if siblingFile.PreviousPath == "" || siblingFile.PreviousPath == siblingFile.Path ||
					(siblingFile.PreviousPath != conflict.Path && siblingFile.Path != conflict.Path) {
					continue
				}
				seen[key] = true
				collisions = append(collisions, structuralCollision{
					SiblingNumber: siblingNumber,
					Path:          conflict.Path,
					Function:      strings.TrimSpace(conflict.Scope),
					CurrentHunk:   currentHunk.Text,
					SiblingHunk: fmt.Sprintf(
						"diff --git a/%s b/%s\nrename from %s\nrename to %s",
						siblingFile.PreviousPath, siblingFile.Path,
						siblingFile.PreviousPath, siblingFile.Path,
					),
				})
				break
			}
		}
	}
	return collisions
}

func gitIsAncestor(dir, ancestor, descendant string) (bool, error) {
	cmd := exec.Command("git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = dir
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, gitOutputError("git merge-base --is-ancestor "+ancestor+" "+descendant, err)
}

func fetchBranchObjects(dir, branch, commit, token string) error {
	url, err := originURL(dir)
	if err != nil {
		return err
	}
	fetch := exec.Command("git", "fetch", url, "refs/heads/"+branch)
	fetch.Dir = dir
	fetch.Env = gitAuthEnv(token)
	if out, err := fetch.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch base %s: %w: %s", branch, err, strings.TrimSpace(string(out)))
	}
	verify := exec.Command("git", "cat-file", "-e", commit+"^{commit}")
	verify.Dir = dir
	if err := verify.Run(); err != nil {
		return gitOutputError("git cat-file -e "+commit+"^{commit}", err)
	}
	return nil
}

func commitIntroducedBetween(dir, commit, oldBase, liveBase string) (bool, error) {
	if commit == "" {
		return false, nil
	}
	inLive, err := gitIsAncestor(dir, commit, liveBase)
	if err != nil || !inLive {
		return false, err
	}
	inOld, err := gitIsAncestor(dir, commit, oldBase)
	if err != nil {
		return false, err
	}
	return !inOld, nil
}

func goFunctionAtRevision(dir, revision, path, function string) (goFunctionSnapshot, bool, error) {
	lsTree := exec.Command("git", "ls-tree", "-z", revision, "--", path)
	lsTree.Dir = dir
	entry, err := lsTree.Output()
	if err != nil {
		return goFunctionSnapshot{}, false, gitOutputError("git ls-tree "+revision+" -- "+path, err)
	}
	if len(entry) == 0 {
		return goFunctionSnapshot{}, false, nil
	}
	show := exec.Command("git", "show", revision+":"+path)
	show.Dir = dir
	source, err := show.Output()
	if err != nil {
		return goFunctionSnapshot{}, false, gitOutputError("git show "+revision+":"+path, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, source, parser.SkipObjectResolution)
	if err != nil {
		return goFunctionSnapshot{}, false, fmt.Errorf("parse %s at %s: %w", path, revision, err)
	}
	for _, declaration := range file.Decls {
		decl, ok := declaration.(*ast.FuncDecl)
		if !ok || functionDeclKey(fset, source, decl) != function {
			continue
		}
		bodyFrom, bodyTo := fset.Position(decl.Pos()).Offset, fset.Position(decl.End()).Offset
		if decl.Body != nil {
			bodyFrom = fset.Position(decl.Body.Lbrace).Offset
			bodyTo = fset.Position(decl.Body.Rbrace).Offset
		}
		return goFunctionSnapshot{
			Lines: normalizedSourceLines(source[bodyFrom:bodyTo]),
		}, true, nil
	}
	return goFunctionSnapshot{}, false, nil
}

func functionDeclKey(fset *token.FileSet, source []byte, decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
	}
	receiver := decl.Recv.List[0].Type
	from := fset.Position(receiver.Pos()).Offset
	to := fset.Position(receiver.End()).Offset
	name := strings.TrimLeft(strings.TrimSpace(string(source[from:to])), "*[]")
	if name == "" {
		return decl.Name.Name
	}
	return name + "." + decl.Name.Name
}

func normalizedSourceLines(source []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(source), "\n") {
		normalized := strings.Join(strings.Fields(line), " ")
		if normalized != "" && normalized != "{" && normalized != "}" {
			lines = append(lines, normalized)
		}
	}
	return lines
}

func sourceLineSimilarity(a, b []string) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	counts := make(map[string]int, len(a))
	for _, line := range a {
		counts[line]++
	}
	intersection := 0
	for _, line := range b {
		if counts[line] > 0 {
			intersection++
			counts[line]--
		}
	}
	return float64(2*intersection) / float64(len(a)+len(b))
}

func functionStructurallyChangedPaths(
	dir, oldBase, liveBase, oldPath, livePath, function string,
) (bool, error) {
	oldFunction, oldFound, err := goFunctionAtRevision(dir, oldBase, oldPath, function)
	if err != nil || !oldFound {
		return false, err
	}
	liveFunction, liveFound, err := goFunctionAtRevision(dir, liveBase, livePath, function)
	if err != nil {
		return false, err
	}
	if !liveFound {
		return true, nil
	}
	oldSize, liveSize := len(oldFunction.Lines), len(liveFunction.Lines)
	delta := oldSize - liveSize
	if delta < 0 {
		delta = -delta
	}
	similarity := sourceLineSimilarity(oldFunction.Lines, liveFunction.Lines)
	largeBoundaryChange := oldSize >= 6 && delta*100 >= oldSize*35
	return (largeBoundaryChange && similarity < 0.75) ||
		(oldSize >= 10 && similarity < 0.35), nil
}

func hydrateCurrentPatches(dir, base string, files []providers.ChangedFile) ([]providers.ChangedFile, error) {
	out := append([]providers.ChangedFile(nil), files...)
	for i := range out {
		if out[i].Patch != "" {
			continue
		}
		args := []string{"diff", "--no-color", "--function-context", base + "...HEAD", "--"}
		if out[i].PreviousPath != "" {
			args = append(args, out[i].PreviousPath)
		}
		args = append(args, out[i].Path)
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		patch, err := cmd.Output()
		if err != nil {
			return nil, gitOutputError("git "+strings.Join(args, " "), err)
		}
		out[i].Patch = string(patch)
	}
	return out, nil
}

func changedFilesTouchPath(files []providers.ChangedFile, path string) bool {
	for _, file := range files {
		if file.Path == path || file.PreviousPath == path {
			return true
		}
	}
	return false
}

func findStructuralCollisions(
	ctx context.Context,
	provider *providers.GitHubProvider,
	repo providers.RepositoryRef,
	selected providers.PullRequestSummary,
	base, headPrefix string,
	conflicts []rebaseConflictLocation,
	dir, pushToken, rebaseBaseSHA string,
) ([]structuralCollision, error) {
	var functionConflicts []rebaseConflictLocation
	for _, conflict := range conflicts {
		if functionScopeKey(conflict.Scope) != "" {
			functionConflicts = append(functionConflicts, conflict)
		}
	}
	if len(functionConflicts) == 0 || rebaseBaseSHA == "" {
		return nil, nil
	}
	if err := fetchBranchObjects(dir, base, rebaseBaseSHA, pushToken); err != nil {
		return nil, err
	}
	liveBase := rebaseBaseSHA
	currentFiles, err := provider.PullRequestFiles(ctx, repo, strconv.Itoa(selected.Number))
	if err != nil {
		return nil, fmt.Errorf("list files for selected PR #%d: %w", selected.Number, err)
	}
	currentFiles, err = hydrateCurrentPatches(dir, selected.BaseSHA, currentFiles)
	if err != nil {
		return nil, fmt.Errorf("load full patch for selected PR #%d: %w", selected.Number, err)
	}
	closedPRs, err := provider.ListRecentlyClosedPullRequests(ctx, providers.ListPullRequestsRequest{
		Repository: repo, Base: base, HeadPrefix: headPrefix,
	}, time.Now().UTC().Add(-siblingOverlapLookback))
	if err != nil {
		return nil, fmt.Errorf("list recently closed pull requests: %w", err)
	}
	sort.Slice(closedPRs, func(i, j int) bool { return closedPRs[i].Number < closedPRs[j].Number })

	var collisions []structuralCollision
	for _, sibling := range closedPRs {
		if sibling.Number == selected.Number || !sibling.Merged {
			continue
		}
		introduced, err := commitIntroducedBetween(dir, sibling.MergeSHA, selected.BaseSHA, liveBase)
		if err != nil {
			return nil, fmt.Errorf("locate merged sibling PR #%d in base history: %w", sibling.Number, err)
		}
		if !introduced {
			continue
		}
		files, err := provider.PullRequestFiles(ctx, repo, strconv.Itoa(sibling.Number))
		if err != nil {
			return nil, fmt.Errorf("list files for merged sibling PR #%d: %w", sibling.Number, err)
		}
		var siblingStructuralConflicts []rebaseConflictLocation
		for _, conflict := range functionConflicts {
			oldPath, livePath := conflict.Path, conflict.Path
			for _, file := range files {
				if file.PreviousPath == conflict.Path {
					oldPath, livePath = file.PreviousPath, file.Path
					break
				}
				if file.Path == conflict.Path && file.PreviousPath != "" {
					oldPath, livePath = file.PreviousPath, file.Path
					break
				}
			}
			changed, err := functionStructurallyChangedPaths(
				dir, selected.BaseSHA, liveBase, oldPath, livePath,
				functionScopeKey(conflict.Scope),
			)
			if err != nil {
				return nil, fmt.Errorf("inspect merged sibling PR #%d function change: %w", sibling.Number, err)
			}
			if changed {
				resolved := conflict
				resolved.Path = oldPath
				if !changedFilesTouchPath(currentFiles, oldPath) && changedFilesTouchPath(currentFiles, livePath) {
					resolved.Path = livePath
				}
				siblingStructuralConflicts = append(siblingStructuralConflicts, resolved)
			}
		}
		if len(siblingStructuralConflicts) == 0 {
			continue
		}
		collisions = append(collisions, matchStructuralCollisions(
			siblingStructuralConflicts, currentFiles, sibling.Number, files,
		)...)
		if len(collisions) >= maxStructuralCollisionHunks {
			return collisions[:maxStructuralCollisionHunks], nil
		}
	}
	return collisions, nil
}

func renderStructuralCollisionContext(selectedNumber int, collisions []structuralCollision) string {
	var sections []string
	for _, collision := range collisions {
		sections = append(sections, fmt.Sprintf(
			"Conflict in `%s` within `%s` after merged sibling PR #%d restructured the same function.\n\n"+
				"<details><summary>PR #%d relevant hunk</summary>\n\n````diff\n%s\n````\n</details>\n\n"+
				"<details><summary>Merged sibling PR #%d relevant hunk</summary>\n\n````diff\n%s\n````\n</details>",
			collision.Path, collision.Function, collision.SiblingNumber,
			selectedNumber, boundedDiffHunk(collision.CurrentHunk),
			collision.SiblingNumber, boundedDiffHunk(collision.SiblingHunk),
		))
	}
	return strings.Join(sections, "\n\n")
}

func boundedDiffHunk(hunk string) string {
	const maxLines = 80
	const maxRunes = 3000
	lines := strings.Split(hunk, "\n")
	if len(lines) > maxLines {
		hunk = strings.Join(lines[:maxLines], "\n") + "\n... hunk truncated ..."
	}
	runes := []rune(hunk)
	if len(runes) > maxRunes {
		hunk = string(runes[:maxRunes]) + "\n... hunk truncated ..."
	}
	return hunk
}

func decodeConflictLocations(raw string) ([]rebaseConflictLocation, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var locations []rebaseConflictLocation
	if err := json.Unmarshal([]byte(raw), &locations); err != nil {
		return nil, fmt.Errorf("decode conflictLocations: %w", err)
	}
	for i, location := range locations {
		if strings.TrimSpace(location.Path) == "" {
			return nil, fmt.Errorf("decode conflictLocations: entry %d has no path", i+1)
		}
	}
	return locations, nil
}
