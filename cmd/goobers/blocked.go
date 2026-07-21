package main

import (
	"encoding/json"
	"flag"
	"io"
	"sort"
	"time"
)

// runBlocked is the `goobers blocked` command group (#973): operator-facing
// inspection and clearing of scheduler/blocked.json, the learned
// dependency-block ledger (#552). Its list/clear subcommands are routed by
// the CLI dispatcher; this handler only fields the bare/help/unknown cases.
func runBlocked(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) {
		pf(w, "Usage: goobers blocked <command> [flags] [path]\n\n"+
			"Inspect and clear scheduler/blocked.json, the learned dependency-block\n"+
			"ledger (#552). Reads and writes go through the same claims.lock the\n"+
			"daemon holds, so they are safe against a concurrent tick.\n\n"+
			"Commands:\n"+
			"  list     print the current blocked-item records\n"+
			"  clear    remove one blocked-item record by item id\n")
	}
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
	fs.Usage = func() {
		pf(stderr, "Usage: goobers blocked list [--json] [path]\n\n"+
			"Print the current scheduler/blocked.json records (item id, repository,\n"+
			"record key, blockers, stage, reason, recordedAt), snapshotted under\n"+
			"claims.lock. Default path is \".\". Exit codes: 0 = printed, 2 =\n"+
			"usage/IO error.\n")
	}
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

	if *jsonOutput {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(recs); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		return 0
	}

	if len(recs) == 0 {
		pln(stdout, "no blocked records")
		return 0
	}

	ids := make([]string, 0, len(recs))
	for id := range recs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, key := range ids {
		rec := recs[key]
		itemID := blockedRecordItemID(key, rec)
		if repository := blockedRepositoryIdentity(rec.Repository); repository != "" {
			pf(stdout, "%s\trepository=%s\tkey=%s\tblockers=%v\tstage=%s\treason=%q\trecordedAt=%s\n",
				itemID, repository, key, rec.Blockers, rec.Stage, rec.Reason, rec.RecordedAt.Format(time.RFC3339))
			continue
		}
		pf(stdout, "%s\tblockers=%v\tstage=%s\treason=%q\trecordedAt=%s\n",
			itemID, rec.Blockers, rec.Stage, rec.Reason, rec.RecordedAt.Format(time.RFC3339))
	}
	return 0
}

// runBlockedClear implements `goobers blocked clear <item-id> [path]` (#973):
// remove exactly one record from the blocked-item ledger under claims.lock.
// A unique item id resolves across repository-scoped keys; an exact record key
// from `blocked list` disambiguates same-number items in multiple repositories.
// Exit codes: 0 = cleared, 1 = business error, 2 = usage/IO error.
func runBlockedClear(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("blocked clear", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers blocked clear <item-id> [path]\n\n"+
			"Remove one scheduler/blocked.json record by item id (e.g. \"955\" or\n"+
			"\"pr/955\"), or by the repository-scoped key shown by `blocked list`\n"+
			"when the id is ambiguous. Default path is \".\". Exit codes: 0 =\n"+
			"cleared, 1 = no unique record, 2 = usage/IO error.\n")
	}
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
				if blockedRecordItemID(key, rec) != itemID {
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
		pf(stderr, "error: multiple blocked records for item %q; use the repository-scoped key from `goobers blocked list`\n", itemID)
		return 1
	}
	if !cleared {
		pf(stderr, "error: no blocked record for item %q\n", itemID)
		return 1
	}
	pf(stdout, "cleared blocked record %s\n", itemID)
	return 0
}
