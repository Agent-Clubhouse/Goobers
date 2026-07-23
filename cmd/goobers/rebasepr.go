package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/providers"
	"sigs.k8s.io/yaml"
)

// runRebasePR implements `goobers rebase-pr` (issue #363): pr-remediation's
// rebase-first, finding-driven routing (design doc §5 D3). Routing is never
// rebase-driven: a clean rebase never suppresses a known substantive
// finding, and a rebase conflict is itself substantive.
//
//	rebase result | finding or failing CI? | action
//	clean         | no                     | force-with-lease push, clear label, done
//	clean         | yes                    | needs the agentic chain (not yet wired, see pr-remediation.yaml)
//	unsafe conflict | either               | needs the agentic chain (the conflict IS substantive)
//
// Re-checks out the PR's own branch first (checkoutExistingBranch, shared
// with gather-pr-context): this stage gets its OWN fresh worktree — see
// checkoutExistingBranch's doc comment — so it cannot assume gather-pr-
// context's checkout survived. A conflict made only of distinct entries
// inserted into the same existing line-oriented list is resolved
// mechanically; all other conflicts retain the agentic path.
const rebasePRHelp = "Usage: goobers rebase-pr [path]\n\n" +
	"Check out the selected PR's branch, attempt a rebase onto its base\n" +
	"(force-with-lease is mandatory for the eventual push — never a bare\n" +
	"push), and route on the result: a clean rebase with no substantive\n" +
	"finding or failing CI force-pushes and clears goobers:needs-remediation;\n" +
	"anything else (an unsafe conflict, substantive finding, or failing CI) needs the\n" +
	"agentic remediation chain, reported via the needsAgent output for the\n" +
	"workflow to route on. Requires selectedNumber/head/base\n" +
	"(Task.InputsFrom gather-pr-context's own outputs) and\n" +
	"hasSubstantiveFindings/hasFailingCI. Exit codes: 0 = routed, 1 =\n" +
	"business error, 2 = usage/IO error.\n"

func runRebasePR(args []string, stdout, stderr io.Writer) int {
	ctx, cancel := providerCommandContext()
	defer cancel()

	fs := flag.NewFlagSet("rebase-pr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "rebase-pr")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	pathArg := ""
	if fs.NArg() == 1 {
		pathArg = fs.Arg(0)
	}
	root := providerStageRoot(pathArg)

	selectedNumber := providerInput("selectedNumber", "")
	head := providerInput("head", "")
	base := providerInput("base", "main")
	if selectedNumber == "" || head == "" {
		pf(stderr, "error: selectedNumber and head are required (inputsFrom gather-pr-context's own outputs)\n")
		return 1
	}
	hasSubstantiveFindings := providerInput("hasSubstantiveFindings", "false") == "true"
	hasFailingCI := providerInput("hasFailingCI", "false") == "true"

	repo, err := providerRepo(root)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pushToken, err := providerToken(capability.RepoPush)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if _, err := providerToken(capability.GitHubIssuesWrite); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	preRebaseSHA, err := checkoutExistingBranch(".", head, pushToken)
	if err != nil {
		pf(stderr, "error: checkout PR #%s's branch %q: %v\n", selectedNumber, head, err)
		return 1
	}

	conflict, err := attemptRebase(".", base, pushToken)
	if err != nil {
		pf(stderr, "error: rebase PR #%s onto %q: %v\n", selectedNumber, base, err)
		return 1
	}

	needsAgent := conflict || hasSubstantiveFindings || hasFailingCI
	resultFile := providerInput("resultFile", "rebase-result.json")

	if !conflict && !hasSubstantiveFindings {
		if err := forcePushWithLease(".", head, preRebaseSHA, pushToken); err != nil {
			pf(stderr, "error: force-push rebased PR #%s branch %q: %v\n", selectedNumber, head, err)
			return 1
		}
	}

	if !needsAgent {
		issuesToken, err := providerToken(capability.GitHubIssuesWrite)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		provider := newGitHubProvider(issuesToken)
		if _, err := provider.UpdateWorkItem(ctx, providers.UpdateWorkItemRequest{
			Repository: repo, ID: selectedNumber, RemoveLabels: []string{needsRemediationLabel},
		}); err != nil {
			return failProviderStage(stderr, fmt.Sprintf("clear %s from PR #%s", needsRemediationLabel, selectedNumber), err, "rebase-result.json")
		}
		if err := writeRebaseResult(resultFile, selectedNumber, head, false, false); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		pf(stdout, "PR #%s: clean rebase onto %s, no substantive finding — force-pushed and cleared %s\n", selectedNumber, base, needsRemediationLabel)
		return 0
	}

	if err := writeRebaseResult(resultFile, selectedNumber, head, conflict, needsAgent); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "PR #%s needs agentic remediation (conflict=%v, substantiveFindings=%v, failingCI=%v) — routing to remediation checkpoint\n", selectedNumber, conflict, hasSubstantiveFindings, hasFailingCI)
	return 0
}

// writeRebaseResult echoes selectedNumber/head forward alongside this
// stage's own needsAgent/conflict outcome — Task.InputsFrom resolves
// against the immediately preceding TASK's own Outputs (a gate never
// updates that chain; the gate this stage feeds is proof: apply-verdict's
// own doc comment establishes the same convention), so remediation-
// checkpoint (after rebase-gate) can only read selectedNumber/head if THIS
// stage re-emits them, exactly like gather-sibling-context re-emits
// pr-select's selectedNumber for apply-verdict two hops later.
func writeRebaseResult(resultFile, selectedNumber, head string, conflict, needsAgent bool) error {
	data, err := json.Marshal(map[string]string{
		"selectedNumber": selectedNumber,
		"head":           head,
		"needsAgent":     strconv.FormatBool(needsAgent),
		"conflict":       strconv.FormatBool(conflict),
	})
	if err != nil {
		return fmt.Errorf("marshal rebase result: %w", err)
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", resultFile, err)
	}
	return nil
}

// attemptRebase resolves only the narrow case where both sides added one
// distinct entry to the same existing line-oriented list at an unambiguous
// ancestor position, ordering the base branch's addition before the PR's.
// Every other conflict is aborted cleanly and reported for the existing
// agentic path.
func attemptRebase(dir, base, token string) (conflict bool, err error) {
	url, err := originURL(dir)
	if err != nil {
		return false, err
	}
	auth := gitAuthEnv(token)
	fetch := exec.Command("git", "fetch", url, "refs/heads/"+base)
	fetch.Dir = dir
	fetch.Env = auth
	if out, err := fetch.CombinedOutput(); err != nil {
		return false, fmt.Errorf("fetch base %s: %w: %s", base, err, strings.TrimSpace(string(out)))
	}

	rebase := exec.Command("git", "rebase", "FETCH_HEAD")
	rebase.Dir = dir
	out, rerr := rebase.CombinedOutput()
	if rerr == nil {
		return false, nil
	}

	for {
		status, resolveErr := resolveAdjacentLineConflicts(dir)
		if resolveErr != nil {
			return false, abortRebaseAfterError(dir, resolveErr)
		}
		if status != rebaseConflictResolved {
			if err := abortRebase(dir); err != nil {
				return false, fmt.Errorf("git rebase FETCH_HEAD: %w: %s; %w", rerr, strings.TrimSpace(string(out)), err)
			}
			if status == rebaseConflictAbsent {
				return false, fmt.Errorf("git rebase FETCH_HEAD: %w: %s", rerr, strings.TrimSpace(string(out)))
			}
			return true, nil
		}

		cont := exec.Command("git", "rebase", "--continue")
		cont.Dir = dir
		cont.Env = append(os.Environ(), "GIT_EDITOR=true")
		continueOut, continueErr := cont.CombinedOutput()
		if continueErr == nil {
			return false, nil
		}

		nextStatus, statusErr := unmergedConflictStatus(dir)
		if statusErr != nil {
			return false, abortRebaseAfterError(dir, statusErr)
		}
		if nextStatus == rebaseConflictAbsent {
			rebaseErr := fmt.Errorf("git rebase --continue: %w: %s", continueErr, strings.TrimSpace(string(continueOut)))
			return false, abortRebaseAfterError(dir, rebaseErr)
		}
		out, rerr = continueOut, continueErr
	}
}

type rebaseConflictStatus uint8

const (
	rebaseConflictAbsent rebaseConflictStatus = iota
	rebaseConflictUnsafe
	rebaseConflictResolved
)

type rebaseConflictStage struct {
	mode string
	oid  string
}

type rebaseConflictFile struct {
	path   string
	stages map[int]rebaseConflictStage
}

type rebaseResolution struct {
	path string
	data []byte
}

// resolveAdjacentLineConflicts inspects Git's three index stages rather than
// conflict-marker text so repository merge-marker configuration cannot widen
// what is considered safe.
func resolveAdjacentLineConflicts(dir string) (rebaseConflictStatus, error) {
	files, err := unmergedConflictFiles(dir)
	if err != nil {
		return rebaseConflictAbsent, err
	}
	if len(files) == 0 {
		return rebaseConflictAbsent, nil
	}

	resolutions := make([]rebaseResolution, 0, len(files))
	for _, file := range files {
		ancestor, hasAncestor := file.stages[1]
		upstream, hasUpstream := file.stages[2]
		incoming, hasIncoming := file.stages[3]
		if !hasAncestor || !hasUpstream || !hasIncoming ||
			ancestor.mode != upstream.mode || ancestor.mode != incoming.mode ||
			(ancestor.mode != "100644" && ancestor.mode != "100755") {
			return rebaseConflictUnsafe, nil
		}
		standardText, err := hasStandardTextMergeAttributes(dir, file.path)
		if err != nil {
			return rebaseConflictAbsent, fmt.Errorf("check merge attributes for %q: %w", file.path, err)
		}
		if !standardText {
			return rebaseConflictUnsafe, nil
		}

		ancestorData, err := readGitBlob(dir, ancestor.oid)
		if err != nil {
			return rebaseConflictAbsent, fmt.Errorf("read ancestor for %q: %w", file.path, err)
		}
		upstreamData, err := readGitBlob(dir, upstream.oid)
		if err != nil {
			return rebaseConflictAbsent, fmt.Errorf("read base branch version for %q: %w", file.path, err)
		}
		incomingData, err := readGitBlob(dir, incoming.oid)
		if err != nil {
			return rebaseConflictAbsent, fmt.Errorf("read PR version for %q: %w", file.path, err)
		}
		merged, ok := mergeAdjacentLineInsertions(file.path, ancestorData, upstreamData, incomingData)
		if !ok {
			return rebaseConflictUnsafe, nil
		}
		resolutions = append(resolutions, rebaseResolution{path: file.path, data: merged})
	}

	for _, resolution := range resolutions {
		path, err := worktreeConflictPath(dir, resolution.path)
		if err != nil {
			return rebaseConflictAbsent, err
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return rebaseConflictAbsent, fmt.Errorf("open conflict path %q: %w", resolution.path, err)
		}
		if _, err := file.Write(resolution.data); err != nil {
			_ = file.Close()
			return rebaseConflictAbsent, fmt.Errorf("write conflict path %q: %w", resolution.path, err)
		}
		if err := file.Close(); err != nil {
			return rebaseConflictAbsent, fmt.Errorf("close conflict path %q: %w", resolution.path, err)
		}

		add := exec.Command("git", "--literal-pathspecs", "add", "--", resolution.path)
		add.Dir = dir
		if addOut, err := add.CombinedOutput(); err != nil {
			return rebaseConflictAbsent, fmt.Errorf("stage resolved path %q: %w: %s", resolution.path, err, strings.TrimSpace(string(addOut)))
		}
	}
	remaining, err := unmergedConflictFiles(dir)
	if err != nil {
		return rebaseConflictAbsent, err
	}
	if len(remaining) != 0 {
		return rebaseConflictAbsent, fmt.Errorf("stage resolved conflicts: %d paths remain unmerged", len(remaining))
	}
	return rebaseConflictResolved, nil
}

func unmergedConflictStatus(dir string) (rebaseConflictStatus, error) {
	files, err := unmergedConflictFiles(dir)
	if err != nil {
		return rebaseConflictAbsent, err
	}
	if len(files) == 0 {
		return rebaseConflictAbsent, nil
	}
	return rebaseConflictUnsafe, nil
}

func unmergedConflictFiles(dir string) ([]rebaseConflictFile, error) {
	cmd := exec.Command("git", "ls-files", "--unmerged", "-z")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("list unmerged paths: %w", err)
	}

	var files []rebaseConflictFile
	byPath := make(map[string]int)
	for _, record := range bytes.Split(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		header, pathBytes, ok := bytes.Cut(record, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("parse unmerged index entry %q", record)
		}
		fields := strings.Fields(string(header))
		if len(fields) != 3 {
			return nil, fmt.Errorf("parse unmerged index header %q", header)
		}
		stage, err := strconv.Atoi(fields[2])
		if err != nil || stage < 1 || stage > 3 {
			return nil, fmt.Errorf("parse unmerged index stage %q", fields[2])
		}
		path := string(pathBytes)
		index, ok := byPath[path]
		if !ok {
			index = len(files)
			byPath[path] = index
			files = append(files, rebaseConflictFile{path: path, stages: make(map[int]rebaseConflictStage, 3)})
		}
		if _, duplicate := files[index].stages[stage]; duplicate {
			return nil, fmt.Errorf("duplicate unmerged index stage %d for %q", stage, path)
		}
		files[index].stages[stage] = rebaseConflictStage{mode: fields[0], oid: fields[1]}
	}
	return files, nil
}

func hasStandardTextMergeAttributes(dir, path string) (bool, error) {
	cmd := exec.Command(
		"git", "check-attr", "-z",
		"binary", "text", "diff", "merge",
		"filter", "ident", "working-tree-encoding",
		"--", path,
	)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	parts := bytes.Split(out, []byte{0})
	if len(parts) == 0 || len(parts[len(parts)-1]) != 0 {
		return false, fmt.Errorf("malformed git check-attr output")
	}
	parts = parts[:len(parts)-1]
	if len(parts)%3 != 0 {
		return false, fmt.Errorf("malformed git check-attr output")
	}

	values := make(map[string]string, 4)
	for i := 0; i < len(parts); i += 3 {
		if string(parts[i]) != path {
			return false, fmt.Errorf("unexpected path %q in git check-attr output", parts[i])
		}
		values[string(parts[i+1])] = string(parts[i+2])
	}
	if values["binary"] != "unspecified" && values["binary"] != "unset" {
		return false, nil
	}
	switch values["text"] {
	case "unspecified", "set", "auto":
	default:
		return false, nil
	}
	switch values["diff"] {
	case "unspecified", "set":
	default:
		return false, nil
	}
	switch values["merge"] {
	case "unspecified", "set", "text":
	default:
		return false, nil
	}
	for _, attribute := range []string{"filter", "ident", "working-tree-encoding"} {
		if values[attribute] != "unspecified" && values[attribute] != "unset" {
			return false, nil
		}
	}
	return true, nil
}

func readGitBlob(dir, oid string) ([]byte, error) {
	cmd := exec.Command("git", "cat-file", "blob", oid)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

func worktreeConflictPath(dir, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe conflict path %q", name)
	}
	return filepath.Join(dir, clean), nil
}

func mergeAdjacentLineInsertions(path string, ancestor, upstream, incoming []byte) ([]byte, bool) {
	if len(ancestor) == 0 ||
		bytes.IndexByte(ancestor, 0) >= 0 ||
		bytes.IndexByte(upstream, 0) >= 0 ||
		bytes.IndexByte(incoming, 0) >= 0 ||
		!utf8.Valid(ancestor) ||
		!utf8.Valid(upstream) ||
		!utf8.Valid(incoming) {
		return nil, false
	}

	ancestorLines := splitFileLines(ancestor)
	upstreamLines := splitFileLines(upstream)
	incomingLines := splitFileLines(incoming)
	upstreamAt, upstreamLine, ok := singleInsertedLine(ancestorLines, upstreamLines)
	if !ok {
		return nil, false
	}
	incomingAt, incomingLine, ok := singleInsertedLine(ancestorLines, incomingLines)
	if !ok || upstreamAt != incomingAt ||
		!strings.HasSuffix(upstreamLine, "\n") ||
		!strings.HasSuffix(incomingLine, "\n") ||
		strings.TrimSpace(upstreamLine) == "" ||
		strings.TrimSpace(upstreamLine) == strings.TrimSpace(incomingLine) ||
		leadingWhitespace(upstreamLine) != leadingWhitespace(incomingLine) ||
		!hasVerifiedMarkerListSyntax(path, ancestor, upstream, incoming, upstreamLine) ||
		!sameAdjacentList(ancestorLines, upstreamAt, upstreamLine, incomingLine) {
		return nil, false
	}

	merged := make([]string, 0, len(ancestorLines)+2)
	merged = append(merged, ancestorLines[:upstreamAt]...)
	merged = append(merged, upstreamLine, incomingLine)
	merged = append(merged, ancestorLines[upstreamAt:]...)
	return []byte(strings.Join(merged, "")), true
}

func hasVerifiedMarkerListSyntax(path string, ancestor, upstream, incoming []byte, insertedLine string) bool {
	kind := listEntryKind(insertedLine)
	if kind == "" {
		return false
	}
	if strings.HasPrefix(kind, "quoted ") {
		return true
	}
	if kind != "- " {
		return false
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext != ".yaml" && ext != ".yml" {
		return false
	}
	for _, data := range [][]byte{ancestor, upstream, incoming} {
		var document any
		if err := yaml.Unmarshal(data, &document); err != nil {
			return false
		}
	}
	return true
}

func splitFileLines(data []byte) []string {
	lines := strings.SplitAfter(string(data), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func singleInsertedLine(ancestor, side []string) (int, string, bool) {
	if len(side) != len(ancestor)+1 {
		return 0, "", false
	}

	prefix := 0
	for prefix < len(ancestor) && ancestor[prefix] == side[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(ancestor) &&
		ancestor[len(ancestor)-1-suffix] == side[len(side)-1-suffix] {
		suffix++
	}
	insertAt := len(ancestor) - suffix
	if insertAt != prefix {
		return 0, "", false
	}
	return insertAt, side[insertAt], true
}

func leadingWhitespace(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

func sameAdjacentList(ancestor []string, insertAt int, upstream, incoming string) bool {
	kind := listEntryKind(upstream)
	if kind == "" || listEntryKind(incoming) != kind {
		return false
	}
	indent := leadingWhitespace(upstream)
	if strings.HasPrefix(kind, "quoted ") {
		if !hasQuotedListContainer(ancestor, insertAt, indent) {
			return false
		}
	} else if !hasMarkerListContainer(ancestor, insertAt, indent) {
		return false
	}
	for _, neighbor := range []int{insertAt - 1, insertAt} {
		if neighbor >= 0 && neighbor < len(ancestor) &&
			leadingWhitespace(ancestor[neighbor]) == indent &&
			listEntryKind(ancestor[neighbor]) == kind {
			return true
		}
	}
	return false
}

func hasMarkerListContainer(ancestor []string, insertAt int, entryIndent string) bool {
	if entryIndent == "" {
		return false
	}
	for i := insertAt - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(ancestor[i])
		if trimmed == "" {
			continue
		}
		indent := leadingWhitespace(ancestor[i])
		if len(indent) >= len(entryIndent) {
			continue
		}
		return strings.HasPrefix(entryIndent, indent) && strings.HasSuffix(trimmed, ":")
	}
	return false
}

func hasQuotedListContainer(ancestor []string, insertAt int, entryIndent string) bool {
	openerIndent := ""
	foundOpener := false
	for i := insertAt - 1; i >= 0; i-- {
		indent := leadingWhitespace(ancestor[i])
		if len(indent) >= len(entryIndent) || strings.TrimSpace(ancestor[i]) == "" {
			continue
		}
		if !strings.HasSuffix(strings.TrimSpace(ancestor[i]), "[") {
			return false
		}
		openerIndent = indent
		foundOpener = true
		break
	}
	if !foundOpener {
		return false
	}

	for i := insertAt; i < len(ancestor); i++ {
		indent := leadingWhitespace(ancestor[i])
		if len(indent) >= len(entryIndent) || strings.TrimSpace(ancestor[i]) == "" {
			continue
		}
		trimmed := strings.TrimSpace(ancestor[i])
		return indent == openerIndent && (trimmed == "]" || trimmed == "],")
	}
	return false
}

func listEntryKind(line string) string {
	line = strings.TrimSpace(line)
	for _, marker := range []string{"- ", "* ", "+ "} {
		if strings.HasPrefix(line, marker) {
			return marker
		}
	}
	for i := 0; i < len(line); i++ {
		if line[i] < '0' || line[i] > '9' {
			if i > 0 && len(line) > i+1 &&
				(line[i] == '.' || line[i] == ')') && line[i+1] == ' ' {
				return "ordered"
			}
			break
		}
	}
	if len(line) >= 3 && line[len(line)-1] == ',' {
		quote := line[0]
		switch quote {
		case '"', '\'', '`':
			for i := 1; i < len(line); i++ {
				if quote != '`' && line[i] == '\\' {
					i++
					continue
				}
				if line[i] == quote {
					if i == len(line)-2 {
						return "quoted " + string(quote)
					}
					return ""
				}
			}
		}
	}
	return ""
}

func abortRebase(dir string) error {
	abort := exec.Command("git", "rebase", "--abort")
	abort.Dir = dir
	if out, err := abort.CombinedOutput(); err != nil {
		return fmt.Errorf("abort rebase: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func abortRebaseAfterError(dir string, cause error) error {
	if err := abortRebase(dir); err != nil {
		return fmt.Errorf("%w; %w", cause, err)
	}
	return cause
}

// forcePushWithLease pushes branch to origin with an explicit
// --force-with-lease=<branch>:<expectedSHA> (design doc §5: "mandatory —
// even in a goober-authored repo a human may push to a branch; the lease
// makes Goobers lose gracefully and re-select next tick rather than clobber
// the push"), authenticated via gitAuthEnv, shared with push-branch's plain
// gitPushBranch (#237) — never a URL-embedded or persisted credential. A
// rebase rewrites history, so push-branch's own non-force push (correct for
// implementation's linear-commit flow) would always be rejected here.
//
// expectedSHA MUST be the branch's remote tip captured at checkout time
// (checkoutExistingBranch's own return value) — NOT re-resolved here right
// before pushing. Re-resolving immediately before the push would make the
// lease tautological (it would always match whatever just landed on the
// remote, silently defeating the "refuse if something pushed since I
// started" guarantee this function exists for — caught by
// TestRebasePRForceWithLeaseRefusesOnConcurrentPush). Plain
// --force-with-lease (no explicit expected value) isn't an option either:
// this binary fetches by resolved URL, not the named "origin" remote
// (originURL's own doc comment explains why — a mirrored remote can't take
// an explicit refspec), so no refs/remotes/origin/<branch> tracking ref is
// ever updated for the bare flag to compare against, which misreports every
// push as "stale info" regardless of whether the remote actually moved.
func forcePushWithLease(dir, branch, expectedSHA, token string) error {
	url, err := originURL(dir)
	if err != nil {
		return err
	}
	cmd := exec.Command("git", "push", "--force-with-lease="+branch+":"+expectedSHA, url, branch+":"+branch)
	cmd.Dir = dir
	cmd.Env = gitAuthEnv(token)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
