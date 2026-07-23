package main

import (
	"encoding/json"
	"flag"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const blockedHelp = "Usage: goobers blocked <command> [flags] [path]\n\n" +
	"Inspect and clear scheduler/blocked.json, the learned dependency-block\n" +
	"ledger (#552). Reads and writes go through the same claims.lock the\n" +
	"daemon holds, so they are safe against a concurrent tick.\n\n" +
	"Commands:\n" +
	"  list     print the current blocked-item records\n" +
	"  clear    remove one blocked-item record by item id\n"

const blockedListHelp = "Usage: goobers blocked list [--json] [path]\n\n" +
	"Print one dependency line per scheduler/blocked.json record, snapshotted\n" +
	"under claims.lock. A single-repository ledger uses #N (or PR #N); when\n" +
	"multiple repositories are present, refs use owner/repo#N. --json emits a\n" +
	"stable ordered array with ref, kind (issue or pull_request), and blockedBy.\n" +
	"Default path is \".\". Exit codes: 0 = printed, 2 = usage/IO error.\n"

const blockedClearHelp = "Usage: goobers blocked clear <item-id> [path]\n\n" +
	"Remove one scheduler/blocked.json record by item id (e.g. \"955\" or\n" +
	"\"pr/955\"), or by the owner/repo#N ref shown by `blocked list`\n" +
	"when the id is ambiguous. Default path is \".\". Exit codes: 0 =\n" +
	"cleared, 1 = no unique record, 2 = usage/IO error.\n"

const (
	blockedReferenceKindIssue       = "issue"
	blockedReferenceKindPullRequest = "pull_request"
)

type blockedListReference struct {
	Ref  string `json:"ref"`
	Kind string `json:"kind"`
}

type blockedListRecord struct {
	Ref       string                 `json:"ref"`
	Kind      string                 `json:"kind"`
	BlockedBy []blockedListReference `json:"blockedBy"`
}

type normalizedBlockedReference struct {
	repository string
	number     string
	kind       string
}

func blockedListRecords(recs map[string]blockedRecord) []blockedListRecord {
	type normalizedRecord struct {
		item       normalizedBlockedReference
		blockedBy  []normalizedBlockedReference
		recordKey  string
		repository string
	}

	normalized := make([]normalizedRecord, 0, len(recs))
	repositories := make(map[string]bool)
	for recordKey, rec := range recs {
		repository := blockedDisplayRepository(recordKey, rec)
		if repository != "" {
			repositories[repository] = true
		}
		blockedBy := make([]normalizedBlockedReference, 0, len(rec.Blockers))
		for _, blocker := range rec.Blockers {
			blockedBy = append(blockedBy, normalizeBlockedReference(blocker, repository))
		}
		normalized = append(normalized, normalizedRecord{
			item:       normalizeBlockedReference(blockedDisplayItemID(recordKey, rec), repository),
			blockedBy:  blockedBy,
			recordKey:  recordKey,
			repository: repository,
		})
	}

	qualifyRepository := len(repositories) > 1
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].repository != normalized[j].repository {
			return normalized[i].repository < normalized[j].repository
		}
		if normalized[i].item.number != normalized[j].item.number {
			return blockedNumberLess(normalized[i].item.number, normalized[j].item.number)
		}
		if normalized[i].item.kind != normalized[j].item.kind {
			return normalized[i].item.kind < normalized[j].item.kind
		}
		return normalized[i].recordKey < normalized[j].recordKey
	})

	records := make([]blockedListRecord, 0, len(normalized))
	for _, record := range normalized {
		sort.SliceStable(record.blockedBy, func(i, j int) bool {
			if record.blockedBy[i].number != record.blockedBy[j].number {
				return blockedNumberLess(record.blockedBy[i].number, record.blockedBy[j].number)
			}
			return record.blockedBy[i].kind < record.blockedBy[j].kind
		})
		blockedBy := make([]blockedListReference, 0, len(record.blockedBy))
		for _, blocker := range record.blockedBy {
			blockedBy = append(blockedBy, blocker.display(qualifyRepository))
		}
		item := record.item.display(qualifyRepository)
		records = append(records, blockedListRecord{
			Ref:       item.Ref,
			Kind:      item.Kind,
			BlockedBy: blockedBy,
		})
	}
	return records
}

func blockedDisplayRepository(recordKey string, rec blockedRecord) string {
	identity := blockedRepositoryIdentity(rec.Repository)
	if identity == "" {
		if separator := strings.LastIndex(recordKey, "#"); separator >= 0 {
			identity = recordKey[:separator]
		}
	}
	if identity == "" {
		return ""
	}
	parts := strings.Split(identity, "/")
	if len(parts) >= 3 {
		parts = parts[1:]
	}
	for i, part := range parts {
		if decoded, err := url.PathUnescape(part); err == nil {
			parts[i] = decoded
		}
	}
	return strings.Join(parts, "/")
}

func blockedDisplayItemID(recordKey string, rec blockedRecord) string {
	if rec.ItemID != "" {
		return rec.ItemID
	}
	return recordKey
}

func normalizeBlockedReference(raw, repository string) normalizedBlockedReference {
	if separator := strings.LastIndex(raw, "#"); separator >= 0 {
		raw = raw[separator+1:]
	}
	if decoded, err := url.PathUnescape(raw); err == nil {
		raw = decoded
	}
	raw = strings.TrimPrefix(raw, "#")
	kind := blockedReferenceKindIssue
	if strings.HasPrefix(raw, pullRequestClaimPrefix) {
		kind = blockedReferenceKindPullRequest
		raw = strings.TrimPrefix(raw, pullRequestClaimPrefix)
	}
	return normalizedBlockedReference{repository: repository, number: raw, kind: kind}
}

func (ref normalizedBlockedReference) display(qualifyRepository bool) blockedListReference {
	display := "#" + ref.number
	if qualifyRepository && ref.repository != "" {
		display = ref.repository + display
	}
	return blockedListReference{Ref: display, Kind: ref.kind}
}

func blockedNumberLess(left, right string) bool {
	leftNumber, leftErr := strconv.ParseUint(left, 10, 64)
	rightNumber, rightErr := strconv.ParseUint(right, 10, 64)
	if leftErr == nil && rightErr != nil {
		return true
	}
	if leftErr != nil && rightErr == nil {
		return false
	}
	if leftErr == nil && leftNumber != rightNumber {
		return leftNumber < rightNumber
	}
	return left < right
}

func humanBlockedReference(ref blockedListReference) string {
	if ref.Kind == blockedReferenceKindPullRequest {
		return "PR " + ref.Ref
	}
	return ref.Ref
}

// runBlocked is the `goobers blocked` command group (#973): operator-facing
// inspection and clearing of scheduler/blocked.json, the learned
// dependency-block ledger (#552). Its list/clear subcommands are routed by
// the CLI dispatcher; this handler only fields the bare/help/unknown cases.
func runBlocked(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) { pf(w, "%s", blockedHelp) }
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		pf(stderr, "error: unknown blocked command %q\n", args[0])
		usage(stderr)
		return 2
	}
}

// runBlockedList implements `goobers blocked list [--json] [path]` (#973): a
// read-only dump of the blocked-item ledger, snapshotted under claims.lock so
// it never reads a record mid-write. Exit codes: 0 = printed (an empty ledger
// is a normal outcome, not an error), 2 = usage/IO error.
func runBlockedList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("blocked list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOutput := fs.Bool("json", false, "emit the records as JSON")
	fs.Usage = helpUsage(stderr, "blocked list")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	recs, err := snapshotBlockedRecords(layoutFor(root))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	records := blockedListRecords(recs)

	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(records); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		return 0
	}

	if len(records) == 0 {
		pln(stdout, "no blocked records")
		return 0
	}

	for _, record := range records {
		blockers := make([]string, 0, len(record.BlockedBy))
		for _, blocker := range record.BlockedBy {
			blockers = append(blockers, humanBlockedReference(blocker))
		}
		pf(stdout, "%s blocked by %s\n",
			humanBlockedReference(blockedListReference{Ref: record.Ref, Kind: record.Kind}),
			strings.Join(blockers, ", "),
		)
	}
	return 0
}

// runBlockedClear implements `goobers blocked clear <item-id> [path]` (#973):
// remove exactly one record from the blocked-item ledger under claims.lock.
// A unique item id resolves across repository-scoped keys; an owner/repo#N ref
// from `blocked list` disambiguates same-number items in multiple repositories.
// Exit codes: 0 = cleared, 1 = business error, 2 = usage/IO error.
func runBlockedClear(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("blocked clear", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "blocked clear")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	itemID := fs.Arg(0)
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}

	cleared := false
	ambiguous := false
	err := updateBlockedRecords(layoutFor(root), func(recs map[string]blockedRecord) bool {
		targetKey := ""
		if _, ok := recs[itemID]; ok {
			targetKey = itemID
		} else {
			for key, rec := range recs {
				qualified := normalizeBlockedReference(blockedDisplayItemID(key, rec), blockedDisplayRepository(key, rec)).display(true)
				if blockedRecordItemID(key, rec) != itemID &&
					qualified.Ref != strings.TrimPrefix(itemID, "PR ") {
					continue
				}
				if targetKey != "" {
					ambiguous = true
					return false
				}
				targetKey = key
			}
		}
		if targetKey == "" {
			return false
		}
		delete(recs, targetKey)
		cleared = true
		return true
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if ambiguous {
		pf(stderr, "error: multiple blocked records for item %q; use the owner/repo#N ref from `goobers blocked list`\n", itemID)
		return 1
	}
	if !cleared {
		pf(stderr, "error: no blocked record for item %q\n", itemID)
		return 1
	}
	pf(stdout, "cleared blocked record %s\n", itemID)
	return 0
}
