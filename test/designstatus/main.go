// Command designstatus validates design-document lifecycle metadata and
// generates the design index.
//
// It replaces a check that proved VOCABULARY — that each document's first
// status word is one of five — with one that proves the status is backed by
// something. #3128 introduced the enum; the 2026-09-06 documentation audit
// found fully shipped designs still marked `draft`, partial implementations
// marked `implemented`, and superseding designs that never marked the records
// they replaced, all while this check passed (#4518).
//
// The header metadata is a blockquote of `> Key: value` lines beside the
// existing Status marker, so no document needs a new frontmatter block:
//
//	# Design: something
//
//	> Status: **implemented** — landed in the CONF wave
//	> Delivered-by: #2074, #2075, #2076
//	> Supersedes: docs/design/older.md
//	> Superseded-by: docs/design/newer.md
//	> Verified: 09db115bb (2026-09-06)
//
// Run with -write to regenerate docs/design/README.md.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// headerLines bounds how far into a document the metadata block may start.
// Raised from 10 when supersession banners pushed some Status markers down.
const headerLines = 40

const (
	designRoot = "docs/design"
	adrRoot    = "docs/adr"
	indexPath  = "docs/design/README.md"
)

var (
	statusMarker = regexp.MustCompile(`(?i)^\s*(?:>\s*)?(?:-\s*)?(?:\*\*status:\*\*|\*\*status:|status:)\s*(?:\*\*)?([a-z]+)\b`)
	// metadataLine matches a `Key: value` header line, with or without the
	// blockquote marker most headers use. The key set is closed below; an
	// unrecognized key is ignored rather than rejected, so ordinary prose in
	// the header block is not mistaken for metadata.
	metadataLine = regexp.MustCompile(`^\s*(?:>\s*)?(?:\*\*)?([A-Za-z][A-Za-z-]*)(?:\*\*)?:\s*(.+?)\s*$`)
	// issueRef matches a #1234 delivery reference.
	issueRef = regexp.MustCompile(`#\d+`)
	// docPath matches a repo-relative docs path inside a metadata value, with
	// or without backticks or Markdown link syntax. It is deliberately not
	// limited to docs/design and docs/adr: a design is sometimes superseded by
	// a requirements spec (docs/design/v0/pr-lifecycle-loop.md by
	// docs/requirements/pr-lifecycle.md), and recording that honestly is worth
	// more than forcing the pointer into a shape the checker prefers.
	docPath = regexp.MustCompile(`docs/[A-Za-z0-9._/-]+\.md`)
	// machineLocalPath matches a path into a home directory. Only some of
	// these are a problem — see isMachineLocalCitation.
	machineLocalPath = regexp.MustCompile(`(?:/Users/|/home/|[A-Za-z]:\\Users\\|~/)[A-Za-z0-9._][A-Za-z0-9._/\\-]*`)

	// serviceAccountHomes are home-directory owners that are not a person. A
	// path under one of these is a product fact that is identical on every
	// machine, not a citation of somebody's checkout.
	serviceAccountHomes = map[string]struct{}{
		"ContainerUser": {}, "ContainerAdministrator": {},
		"runner": {}, "root": {},
	}

	statuses = map[string]struct{}{
		"draft":       {},
		"approved":    {},
		"implemented": {},
		"superseded":  {},
		"historical":  {},
	}
)

// externalEvidenceDisclaimer is the phrase a document must carry when it cites
// a path inside somebody's home directory. Such a citation cannot be resolved
// by any other reader, so the document must say so rather than let a reader
// assume the evidence is reachable. Committing the artifact, or citing a stable
// URL, is better than the disclaimer — the disclaimer is the floor, not the
// goal (#4518, audit theme R8).
const externalEvidenceDisclaimer = "not reproducible from this repository"

// document is one design or ADR page's parsed header.
type document struct {
	Path         string
	Title        string
	Status       string
	StatusDetail string
	DeliveredBy  []string
	Supersedes   []string
	SupersededBy []string
	Verified     string
	Area         string

	// MachineLocalPaths are citations into a home directory, which no other
	// reader can resolve; DisclaimsExternalEvidence records whether the
	// document says so.
	MachineLocalPaths         []string
	DisclaimsExternalEvidence bool
}

func main() {
	write := flag.Bool("write", false, "regenerate the design index instead of checking it")
	flag.Parse()

	docs, problems := loadDocuments(designRoot, adrRoot)

	if *write {
		if err := os.WriteFile(indexPath, []byte(renderIndex(docs)), 0o644); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "designstatus: write index: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("designstatus: wrote %s (%d documents)\n", indexPath, len(docs))
		return
	}

	problems = append(problems, validate(docs)...)
	problems = append(problems, checkIndexIsCurrent(docs)...)
	if len(problems) > 0 {
		sort.Strings(problems)
		_, _ = fmt.Fprintf(os.Stderr, "designstatus:\n%s\n", strings.Join(problems, "\n"))
		os.Exit(1)
	}
	fmt.Printf("designstatus: %d design/ADR documents carry valid, evidence-bearing status metadata\n", len(docs))
}

func loadDocuments(roots ...string) ([]document, []string) {
	var (
		docs     []document
		problems []string
	)
	for _, root := range roots {
		err := filepath.WalkDir(root, func(p string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || strings.ToLower(filepath.Ext(p)) != ".md" {
				return nil
			}
			if filepath.ToSlash(p) == indexPath {
				// The generated index is not itself a design document.
				return nil
			}
			doc, parseErr := parseDocument(p)
			if parseErr != nil {
				problems = append(problems, parseErr.Error())
				return nil
			}
			docs = append(docs, doc)
			return nil
		})
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", root, err))
		}
	}
	sort.Slice(docs, func(i, j int) bool { return docs[i].Path < docs[j].Path })
	return docs, problems
}

func parseDocument(p string) (document, error) {
	file, err := os.Open(p)
	if err != nil {
		return document{}, fmt.Errorf("%s: %w", p, err)
	}
	defer func() { _ = file.Close() }()

	doc := document{Path: filepath.ToSlash(p)}
	body, err := os.ReadFile(p)
	if err != nil {
		return document{}, fmt.Errorf("%s: %w", p, err)
	}
	for _, candidate := range machineLocalPath.FindAllString(string(body), -1) {
		if isMachineLocalCitation(candidate) {
			doc.MachineLocalPaths = append(doc.MachineLocalPaths, candidate)
		}
	}
	// Match with line wrapping and emphasis markers normalised away, so
	// reflowing the paragraph the disclaimer sits in cannot silently disarm
	// the check.
	doc.DisclaimsExternalEvidence = strings.Contains(normalizeProse(string(body)), externalEvidenceDisclaimer)

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for line := 1; line <= headerLines && scanner.Scan(); line++ {
		text := scanner.Text()
		if doc.Title == "" && strings.HasPrefix(text, "# ") {
			doc.Title = strings.TrimSpace(strings.TrimPrefix(text, "# "))
		}
		if doc.Status == "" {
			if match := statusMarker.FindStringSubmatch(text); match != nil {
				doc.Status = strings.ToLower(match[1])
				doc.StatusDetail = strings.TrimSpace(text)
				continue
			}
		}
		match := metadataLine.FindStringSubmatch(text)
		if match == nil {
			continue
		}
		switch strings.ToLower(match[1]) {
		case "delivered-by":
			doc.DeliveredBy = issueRef.FindAllString(match[2], -1)
		case "supersedes":
			doc.Supersedes = docPath.FindAllString(match[2], -1)
		case "superseded-by":
			doc.SupersededBy = docPath.FindAllString(match[2], -1)
		case "verified":
			doc.Verified = match[2]
		case "area":
			doc.Area = match[2]
		}
	}
	if err := scanner.Err(); err != nil {
		return document{}, fmt.Errorf("%s: read: %w", p, err)
	}
	if doc.Status == "" {
		return document{}, fmt.Errorf("%s: missing Status marker in first %d lines", doc.Path, headerLines)
	}
	if doc.Title == "" {
		doc.Title = strings.TrimSuffix(filepath.Base(doc.Path), ".md")
	}
	return doc, nil
}

func validate(docs []document) []string {
	byPath := make(map[string]*document, len(docs))
	for i := range docs {
		byPath[docs[i].Path] = &docs[i]
	}

	var problems []string
	for i := range docs {
		doc := &docs[i]
		if _, ok := statuses[doc.Status]; !ok {
			problems = append(problems, fmt.Sprintf("%s: unknown status %q", doc.Path, doc.Status))
			continue
		}

		// An `implemented` page asserts that work landed. Require it to name
		// what landed, so the claim can be checked instead of trusted. Use
		// `approved` or `draft` for a design whose delivery has not happened,
		// and record a partial delivery by listing what DID land — a partial
		// implementation is representable without overstating it.
		if doc.Status == "implemented" && len(doc.DeliveredBy) == 0 {
			problems = append(problems, fmt.Sprintf(
				"%s: status is `implemented` but no `> Delivered-by:` line names the issues or PRs that delivered it; "+
					"add one (e.g. `> Delivered-by: #2074, #2075`), or use `approved`/`draft` if delivery has not happened", doc.Path))
		}

		// A `superseded` page must say what replaced it, or a reader who lands
		// on it has no way forward.
		if doc.Status == "superseded" && len(doc.SupersededBy) == 0 {
			problems = append(problems, fmt.Sprintf(
				"%s: status is `superseded` but no `> Superseded-by:` line names the document that replaced it", doc.Path))
		}

		problems = append(problems, checkSupersession(doc, byPath)...)

		if len(doc.MachineLocalPaths) > 0 && !doc.DisclaimsExternalEvidence {
			problems = append(problems, fmt.Sprintf(
				"%s cites machine-local path(s) %s that no other reader can resolve. "+
					"Commit the artifact, cite a stable URL, or state %q where the citation appears",
				doc.Path, strings.Join(uniqueSorted(doc.MachineLocalPaths), ", "), externalEvidenceDisclaimer))
		}
	}
	return problems
}

// isMachineLocalCitation reports whether a home-directory path is a citation of
// somebody's own machine rather than a product fact.
//
// Two shapes are deliberately NOT citations, because they name the same
// location on every machine:
//
//   - a dotfile path under the user's home (`~/.copilot/mcp-config.json`) —
//     that is where the tool keeps its config, for everyone; and
//   - a home under a service account (`C:\Users\ContainerUser\AppData\...`) —
//     that is a container identity, not a person.
func isMachineLocalCitation(candidate string) bool {
	normalized := strings.ReplaceAll(candidate, `\`, "/")
	if rest, ok := strings.CutPrefix(normalized, "~/"); ok {
		return !strings.HasPrefix(rest, ".")
	}
	for _, prefix := range []string{"/Users/", "/home/"} {
		if rest, ok := strings.CutPrefix(normalized, prefix); ok {
			owner, _, _ := strings.Cut(rest, "/")
			_, service := serviceAccountHomes[owner]
			return !service
		}
	}
	if index := strings.Index(normalized, ":/Users/"); index >= 0 {
		owner, _, _ := strings.Cut(normalized[index+len(":/Users/"):], "/")
		_, service := serviceAccountHomes[owner]
		return !service
	}
	return true
}

// normalizeProse collapses whitespace and strips Markdown emphasis and
// blockquote markers, so a phrase can be found regardless of how the paragraph
// carrying it happens to be wrapped.
func normalizeProse(markdown string) string {
	var b strings.Builder
	for _, line := range strings.Split(markdown, "\n") {
		b.WriteString(strings.TrimPrefix(strings.TrimSpace(line), "> "))
		b.WriteString(" ")
	}
	collapsed := strings.ReplaceAll(b.String(), "**", "")
	collapsed = strings.ReplaceAll(collapsed, "*", "")
	return strings.Join(strings.Fields(collapsed), " ")
}

func uniqueSorted(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// checkSupersession enforces reciprocity: a supersession recorded on one side
// must be recorded on the other. A one-sided marker is exactly the failure the
// audit found — goobernetes-architecture.md declared two documents superseded
// and neither gained a forward pointer, so a newer design was written against
// the obsolete one.
func checkSupersession(doc *document, byPath map[string]*document) []string {
	var problems []string
	for _, target := range doc.SupersededBy {
		if target == doc.Path {
			problems = append(problems, fmt.Sprintf("%s: `Superseded-by:` names the document itself", doc.Path))
			continue
		}
		other, ok := byPath[target]
		if !ok {
			// Not an indexed design/ADR page. It must still exist — a dangling
			// forward pointer is worse than none — but reciprocity cannot be
			// asked of a document outside this corpus.
			if _, err := os.Stat(target); err != nil {
				problems = append(problems, fmt.Sprintf("%s: `Superseded-by: %s` names a file that does not exist", doc.Path, target))
			}
			continue
		}
		if !contains(other.Supersedes, doc.Path) {
			problems = append(problems, fmt.Sprintf(
				"%s declares `Superseded-by: %s`, but %s has no reciprocal `> Supersedes: %s` line; "+
					"a one-sided supersession leaves the superseding document silent about what it replaced",
				doc.Path, target, target, doc.Path))
		}
	}
	for _, target := range doc.Supersedes {
		if target == doc.Path {
			problems = append(problems, fmt.Sprintf("%s: `Supersedes:` names the document itself", doc.Path))
			continue
		}
		other, ok := byPath[target]
		if !ok {
			if _, err := os.Stat(target); err != nil {
				problems = append(problems, fmt.Sprintf("%s: `Supersedes: %s` names a file that does not exist", doc.Path, target))
			}
			continue
		}
		if !contains(other.SupersededBy, doc.Path) {
			problems = append(problems, fmt.Sprintf(
				"%s declares `Supersedes: %s`, but %s has no reciprocal `> Superseded-by: %s` line; "+
					"a reader landing on the superseded document would get no forward pointer",
				doc.Path, target, target, doc.Path))
		}
	}
	return problems
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func checkIndexIsCurrent(docs []document) []string {
	current, err := os.ReadFile(indexPath)
	if err != nil {
		return []string{fmt.Sprintf("%s: %v — regenerate with `go run ./test/designstatus -write`", indexPath, err)}
	}
	if string(current) != renderIndex(docs) {
		return []string{fmt.Sprintf("%s is out of date; regenerate with `go run ./test/designstatus -write`", indexPath)}
	}
	return nil
}

func renderIndex(docs []document) string {
	var b strings.Builder
	b.WriteString("# Design documents\n\n")
	b.WriteString("<!-- Generated by `go run ./test/designstatus -write`. Do not edit by hand. -->\n\n")
	b.WriteString("Every design document and ADR in this repository, with the lifecycle metadata\n")
	b.WriteString("its own header declares. The `designstatus` check regenerates this file and\n")
	b.WriteString("fails if it differs, so a new design cannot be added without appearing here.\n\n")
	b.WriteString("**Status is a claim, and some of it is checked.** `implemented` requires a\n")
	b.WriteString("`Delivered-by:` line naming what landed; `superseded` requires a\n")
	b.WriteString("`Superseded-by:` line; and a supersession recorded on one document must be\n")
	b.WriteString("recorded reciprocally on the other. Everything else here is the document's own\n")
	b.WriteString("word — read the page, not this table, before depending on it.\n\n")

	counts := map[string]int{}
	for _, doc := range docs {
		counts[doc.Status]++
	}
	b.WriteString("| Status | Documents |\n|---|---:|\n")
	for _, status := range []string{"draft", "approved", "implemented", "superseded", "historical"} {
		fmt.Fprintf(&b, "| `%s` | %d |\n", status, counts[status])
	}
	fmt.Fprintf(&b, "| **Total** | **%d** |\n\n", len(docs))

	groups := map[string][]document{}
	var order []string
	for _, doc := range docs {
		group := path.Dir(doc.Path)
		if _, seen := groups[group]; !seen {
			order = append(order, group)
		}
		groups[group] = append(groups[group], doc)
	}
	sort.Strings(order)

	for _, group := range order {
		fmt.Fprintf(&b, "## `%s/`\n\n", group)
		b.WriteString("| Document | Status | Delivered by | Superseded by | Verified |\n")
		b.WriteString("|---|---|---|---|---|\n")
		for _, doc := range groups[group] {
			link, err := filepath.Rel(path.Dir(indexPath), doc.Path)
			if err != nil {
				link = doc.Path
			}
			fmt.Fprintf(&b, "| [%s](%s) | `%s` | %s | %s | %s |\n",
				escapeCell(doc.Title), filepath.ToSlash(link), doc.Status,
				cellOrDash(strings.Join(doc.DeliveredBy, ", ")),
				cellOrDash(linkList(doc.SupersededBy)),
				cellOrDash(escapeCell(doc.Verified)))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func linkList(paths []string) string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		link, err := filepath.Rel(path.Dir(indexPath), p)
		if err != nil {
			link = p
		}
		out = append(out, fmt.Sprintf("[`%s`](%s)", path.Base(p), filepath.ToSlash(link)))
	}
	return strings.Join(out, ", ")
}

func cellOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func escapeCell(value string) string {
	return strings.ReplaceAll(value, "|", `\|`)
}
