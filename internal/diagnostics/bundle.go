package diagnostics

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Archive file names inside a bundle. They are fixed so a reader — a person or
// a tool — never has to discover the layout.
const (
	FileJSON    = "diagnostics.json"
	FileSummary = "summary.md"
)

// WriteArchive writes bundle as a deterministic gzipped tar containing the
// machine-readable JSON and the human-readable summary.
//
// Determinism is a contract, not a nicety: two collections of the same
// instance state must produce byte-identical archives apart from the injected
// collection time, so a reader can diff two bundles and see only what actually
// changed. Every tar header is therefore fixed — the same mode, a zero
// uid/gid, no uname/gname, and the bundle's own GeneratedAt as the modtime —
// and the gzip header carries no name or timestamp of its own.
func WriteArchive(w io.Writer, bundle Bundle) error {
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return fmt.Errorf("encode diagnostics: %w", err)
	}
	data = append(data, '\n')
	summary := []byte(Summary(bundle))

	modTime := time.Unix(0, 0).UTC()
	if parsed, err := time.Parse(time.RFC3339, bundle.GeneratedAt); err == nil {
		modTime = parsed.UTC()
	}

	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return fmt.Errorf("open gzip writer: %w", err)
	}
	tw := tar.NewWriter(gz)
	for _, entry := range []struct {
		name string
		data []byte
	}{
		{FileJSON, data},
		{FileSummary, summary},
	} {
		header := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     entry.name,
			Mode:     0o644,
			Size:     int64(len(entry.data)),
			ModTime:  modTime,
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(header); err != nil {
			return fmt.Errorf("write %s header: %w", entry.name, err)
		}
		if _, err := tw.Write(entry.data); err != nil {
			return fmt.Errorf("write %s: %w", entry.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	return gz.Close()
}

// Summary renders the human-readable half.
//
// It is ordered by what an operator actually asks, in the order they ask it:
// what was running, was the daemon alive, which config generation, which
// credentials were present, and then — the reason the bundle exists — what
// each run decided and why. Each section is its own function so the ordering
// is the only thing this one expresses.
func Summary(bundle Bundle) string {
	var b strings.Builder
	for _, section := range []func(*strings.Builder, Bundle){
		summaryHeader,
		summaryDaemon,
		summaryConfiguration,
		summaryCredentials,
		summaryRuns,
		summaryNotes,
		summaryContract,
	} {
		section(&b, bundle)
	}
	return b.String()
}

func writeLine(b *strings.Builder, format string, args ...any) {
	fmt.Fprintf(b, format+"\n", args...)
}

func summaryHeader(b *strings.Builder, bundle Bundle) {
	writeLine(b, "# Goobers diagnostics")
	writeLine(b, "")
	writeLine(b, "Collected %s by goobers %s (%s, %s/%s).",
		bundle.GeneratedAt, bundle.Binary.Version, bundle.Binary.Commit, bundle.Binary.OS, bundle.Binary.Arch)
	writeLine(b, "Journal schema v%d. Contract surface: %d built-in stage commands.",
		bundle.Contract.JournalSchemaVersion, len(bundle.Contract.StageCommands))
	writeLine(b, "")
}

func summaryDaemon(b *strings.Builder, bundle Bundle) {
	writeLine(b, "## Daemon")
	writeLine(b, "")
	switch {
	case bundle.Daemon.Running && bundle.Daemon.Stale:
		writeLine(b, "- **Running but STALE**: pid %d has not ticked since %s. A stale holder reads as a live daemon to everything that checks the lock.",
			bundle.Daemon.PID, orUnknown(bundle.Daemon.LastTickAt))
	case bundle.Daemon.Running:
		writeLine(b, "- Running: pid %d, version %s, started %s, last tick %s.",
			bundle.Daemon.PID, orUnknown(bundle.Daemon.Version),
			orUnknown(bundle.Daemon.StartedAt), orUnknown(bundle.Daemon.LastTickAt))
	case bundle.Daemon.LockPresent:
		writeLine(b, "- Not running, but a lock file is present — a previous daemon exited without releasing it.")
	default:
		writeLine(b, "- Not running, and no lock file is present.")
	}
	writeLine(b, "")
}

func summaryConfiguration(b *strings.Builder, bundle Bundle) {
	writeLine(b, "## Configuration")
	writeLine(b, "")
	writeLine(b, "- Instance root: `%s`", bundle.Instance.Root)
	if bundle.Instance.ConfigDigest != "" {
		writeLine(b, "- Config digest: `%s`", bundle.Instance.ConfigDigest)
	}
	for _, gaggle := range bundle.Instance.Gaggles {
		writeLine(b, "- Gaggle `%s`: %d workflow(s), %d goober(s)", gaggle.Name, len(gaggle.Workflows), len(gaggle.Goobers))
		for _, workflow := range gaggle.Workflows {
			writeLine(b, "  - `%s` (dsl %s) definition `%s`",
				workflow.Name, orUnknown(workflow.DSLVersion), orUnknown(workflow.DefinitionDigest))
		}
	}
	writeLine(b, "")
}

func summaryCredentials(b *strings.Builder, bundle Bundle) {
	writeLine(b, "## Credentials")
	writeLine(b, "")
	writeLine(b, "Names and sources only — this bundle never resolves a credential, so it cannot contain one.")
	writeLine(b, "")
	if len(bundle.Credentials) == 0 {
		writeLine(b, "- None declared.")
	}
	for _, cred := range bundle.Credentials {
		writeLine(b, "- `%s` from %s `%s` — %s", cred.Capability, cred.SourceKind, cred.SourceName, presenceLabel(cred))
	}
	writeLine(b, "")
}

func presenceLabel(cred CredentialPresence) string {
	if cred.Present == nil {
		return "presence not determinable without resolving it"
	}
	if *cred.Present {
		return "source present"
	}
	return "source missing"
}

func summaryRuns(b *strings.Builder, bundle Bundle) {
	writeLine(b, "## Runs")
	writeLine(b, "")
	if len(bundle.Runs) == 0 {
		writeLine(b, "- No readable runs in scope.")
	}
	for _, run := range bundle.Runs {
		summaryRun(b, run)
	}
}

func summaryRun(b *strings.Builder, run RunInfo) {
	writeLine(b, "### %s — %s/%s (%s)", run.RunID, run.Gaggle, run.Workflow, orUnknown(run.Phase))
	writeLine(b, "")
	if run.WorkflowDigest != "" {
		writeLine(b, "- Definition: `%s`", run.WorkflowDigest)
	}
	if run.StartedAt != "" {
		writeLine(b, "- Window: %s → %s", run.StartedAt, orUnknown(run.FinishedAt))
	}
	if run.DecisiveError != nil {
		writeLine(b, "- **Decisive error** in `%s` (`%s`): %s",
			orUnknown(run.DecisiveError.Stage), orUnknown(run.DecisiveError.Code), run.DecisiveError.Message)
	}
	if len(run.Decisions) > 0 {
		writeLine(b, "- Selector decisions:")
		for _, decision := range run.Decisions {
			verb := "excluded"
			if decision.Included {
				verb = "selected"
			}
			writeLine(b, "  - `%s` %s (%s): %s", decision.Stage, verb, decision.Subject, decision.Reason)
		}
	}
	if len(run.Stages) > 0 {
		writeLine(b, "- Stages:")
		for _, stage := range run.Stages {
			detail := stage.Status
			if stage.Error != nil {
				detail = fmt.Sprintf("%s — %s: %s", stage.Status, stage.Error.Code, stage.Error.Message)
			}
			writeLine(b, "  - `%s` attempt %d: %s (%d artifact(s))", stage.Stage, stage.Attempt, detail, len(stage.Artifacts))
		}
	}
	writeLine(b, "")
}

func summaryNotes(b *strings.Builder, bundle Bundle) {
	if len(bundle.Notes) == 0 {
		return
	}
	writeLine(b, "## Not collected")
	writeLine(b, "")
	writeLine(b, "Each line is a source the collector could not read. An omission stated is not an omission found.")
	writeLine(b, "")
	for _, note := range bundle.Notes {
		writeLine(b, "- %s", note)
	}
	writeLine(b, "")
}

func summaryContract(b *strings.Builder, bundle Bundle) {
	writeLine(b, "## Stage contract")
	writeLine(b, "")
	writeLine(b, "The built-in stage commands this binary implements, with the capabilities each requires.")
	writeLine(b, "")
	commands := append([]StageCommandInfo(nil), bundle.Contract.StageCommands...)
	sort.Slice(commands, func(i, j int) bool { return commands[i].Command < commands[j].Command })
	for _, command := range commands {
		caps := "none"
		if len(command.Capabilities) > 0 {
			caps = strings.Join(command.Capabilities, ", ")
		}
		writeLine(b, "- `%s` → `%s` (%s)", command.Command, orUnknown(command.ResultFile), caps)
	}
}

func orUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}
