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
			"Print the current scheduler/blocked.json records (item id, blockers,\n"+
			"stage, reason, recordedAt), snapshotted under claims.lock. Default path\n"+
			"is \".\". Exit codes: 0 = printed, 2 = usage/IO error.\n")
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
	for _, id := range ids {
		rec := recs[id]
		pf(stdout, "%s\tblockers=%v\tstage=%s\treason=%q\trecordedAt=%s\n",
			id, rec.Blockers, rec.Stage, rec.Reason, rec.RecordedAt.Format(time.RFC3339))
	}
	return 0
}

// runBlockedClear implements `goobers blocked clear <item-id> [path]` (#973):
// remove exactly one record from the blocked-item ledger under the same
// claims.lock updateBlockedRecords already uses, so it is safe against a
// concurrent daemon tick. The item id is the literal blocked.json key,
// including any "pr/" prefix a pr-remediation escalation recorded (see
// blockedLookupID). Exit codes: 0 = cleared, 1 = business error (no such
// record), 2 = usage/IO error.
func runBlockedClear(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("blocked clear", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		pf(stderr, "Usage: goobers blocked clear <item-id> [path]\n\n"+
			"Remove one scheduler/blocked.json record by its exact item id (the\n"+
			"literal key, e.g. \"955\" or \"pr/955\"), under claims.lock. Default path\n"+
			"is \".\". Exit codes: 0 = cleared, 1 = no such record, 2 = usage/IO\n"+
			"error.\n")
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
	err := updateBlockedRecords(layoutFor(root), func(recs map[string]blockedRecord) bool {
		if _, ok := recs[itemID]; !ok {
			return false
		}
		delete(recs, itemID)
		cleared = true
		return true
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if !cleared {
		pf(stderr, "error: no blocked record for item %q\n", itemID)
		return 1
	}
	pf(stdout, "cleared blocked record %s\n", itemID)
	return 0
}
