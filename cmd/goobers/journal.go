package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

const journalHelp = "Usage: goobers journal <redact> [flags]\n\n" +
	"redact: remove a leaked secret from a run's stored blob and append a\n" +
	"        redaction event — the one sanctioned edit to the append-only\n" +
	"        journal (ARCHITECTURE.md §4, SEC-041).\n"

func runJournal(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) { pf(w, "%s", journalHelp) }
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		pf(stderr, "goobers journal: unknown subcommand %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

const journalRedactHelp = "Usage: goobers journal redact --run <id> --path <blob> --reason <text> [--secret-file <f>] [instance-path]\n\n" +
	"Registers the leaked secret with the run's scrubber, rewrites the target blob\n" +
	"with the secret redacted, removes the leaked bytes from rest, and appends a\n" +
	"redaction event recording old→new digests (ARCHITECTURE.md §4, SEC-041).\n\n" +
	"The secret's exact bytes are read from stdin (or --secret-file) — never a flag —\n" +
	"so the value is not exposed in the process table or shell history. It must match\n" +
	"the leaked bytes exactly (no trailing newline unless the secret has one):\n\n" +
	"  printf %s \"$LEAKED_TOKEN\" | goobers journal redact --run <id> \\\n" +
	"      --path inputs/creds.env --reason 'token pasted into the issue body'\n\n" +
	"Exit codes: 0 = redacted, 1 = nothing redacted / business error, 2 = usage/IO error.\n"

// runJournalRedact implements `goobers journal redact` — the CLI surface for
// journal.Run.Redact. It registers the now-known leaked secret with the run's
// scrubber, rewrites the target blob with the secret redacted, removes the
// leaked bytes from rest, and appends a redaction event recording the old→new
// digests so even the exception leaves a trace (§4).
//
// The secret value is read from stdin (or --secret-file), never a flag, so a
// remediation for a leaked credential does not itself leak it into argv, the
// process table, or shell history.
func runJournalRedact(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("journal redact", flag.ContinueOnError)
	fs.SetOutput(stderr)
	runID := fs.String("run", "", "run id whose stored blob holds the leaked secret (required)")
	blobPath := fs.String("path", "", "journal-relative path of the blob to redact, e.g. inputs/creds.env "+
		"or artifacts/sha256/ab/cd… (required)")
	reason := fs.String("reason", "", "why the redaction was performed, recorded in the redaction event (required)")
	secretFile := fs.String("secret-file", "", "read the exact leaked secret bytes from this file instead of stdin")
	fs.Usage = helpUsage(stderr, "journal redact")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *runID == "" || *blobPath == "" || *reason == "" {
		pf(stderr, "error: --run, --path, and --reason are all required\n\n")
		fs.Usage()
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

	l := instance.NewLayout(root)
	resolvedRunID, err := resolveRunID(l, *runID)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	runDir, err := runDirFor(l, resolvedRunID)
	if err != nil {
		pf(stderr, "error: locate run %q: %v\n", resolvedRunID, err)
		return 2
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		pf(stderr, "error: open run %q: %v\n", resolvedRunID, err)
		return 2
	}
	identity, err := reader.Identity()
	if err != nil {
		pf(stderr, "error: read run %q identity: %v\n", resolvedRunID, err)
		return 2
	}

	secret, err := readSecret(*secretFile)
	if err != nil {
		pf(stderr, "error: read secret: %v\n", err)
		return 2
	}
	if len(secret) == 0 {
		pf(stderr, "error: empty secret — provide the leaked value on stdin or via --secret-file\n")
		return 2
	}

	// Build the target Ref from the bytes currently at rest: a stored blob commits
	// to its own digest, so hashing what is on disk yields the digest the journal
	// recorded (and lets Redact detect a no-op if the secret is not actually
	// present).
	blobFull := filepath.Join(runDir, *blobPath)
	cur, err := os.ReadFile(blobFull)
	if err != nil {
		pf(stderr, "error: read target blob %q in run %q: %v\n", *blobPath, resolvedRunID, err)
		return 2
	}
	target := journal.Ref{Path: *blobPath, Digest: journal.Digest(cur), Size: int64(len(cur))}

	pf(stdout, "run:      %s\n", resolvedRunID)
	pf(stdout, "workflow: %s\n", identity.Workflow)

	// Register the now-known secret so the scrubber catches it, then reopen the run
	// for append and perform the sanctioned edit. Recover is the reopen-for-append
	// path; on a clean run it replays without changing anything.
	reg, scrub := journal.DefaultScrubber()
	reg.Register(secret)
	run, _, err := journal.Recover(runDir, journal.WithScrubber(scrub))
	if err != nil {
		pf(stderr, "error: open run %q: %v\n", resolvedRunID, err)
		return 2
	}
	defer func() { _ = run.Close() }()

	newRef, err := run.Redact(target, *reason)
	if err != nil {
		if errors.Is(err, journal.ErrNothingRedacted) {
			pf(stderr, "nothing redacted: the secret was not found in %s "+
				"(already clean, the value does not match exactly, or it is shorter than the minimum)\n", *blobPath)
			return 1
		}
		pf(stderr, "error: redact %s: %v\n", *blobPath, err)
		return 1
	}
	pf(stdout, "redacted %s\n", *blobPath)
	pf(stdout, "  old digest: %s\n", target.Digest)
	pf(stdout, "  new digest: %s\n", newRef.Digest)
	pf(stdout, "  stored at:  %s\n", newRef.Path)
	return 0
}

// readSecret returns the exact secret bytes from file (if non-empty) or stdin.
func readSecret(file string) ([]byte, error) {
	if file != "" {
		return os.ReadFile(file)
	}
	return io.ReadAll(os.Stdin)
}
