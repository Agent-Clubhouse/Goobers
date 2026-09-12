package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/diagnostics"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/providerstage"
	"github.com/goobers/goobers/internal/supportmatrix"
	"github.com/goobers/goobers/internal/version"
)

const diagnosticsBundleHelp = "Usage: goobers diagnostics bundle [--run <id>] [--pr <number>] " +
	"[--max-runs <n>] [--output <path>] [--json] [path]\n\n" +
	"Collect a portable, redacted support bundle from this binary and an instance\n" +
	"directory alone — no checkout of the Goobers source required, on this machine\n" +
	"or on the one that reads it.\n\n" +
	"The bundle carries the binary's build identity and contract surface (journal\n" +
	"schema, admitted DSL versions, every built-in stage command with the\n" +
	"capabilities it requires), daemon lifecycle, the loaded config generation and\n" +
	"its workflow/goober digests, declared credentials by NAME and SOURCE, and for\n" +
	"each run in scope its stage timeline, artifact metadata, selector decisions,\n" +
	"and decisive error in full.\n\n" +
	"It cannot contain a credential. Values are never resolved and never read:\n" +
	"a credential contributes its capability, source kind and source name only.\n" +
	"Agent transcripts and stage stdout are excluded wholesale rather than\n" +
	"filtered, and artifacts contribute metadata, not content. Everything that\n" +
	"does survive is then passed through the same secret-pattern net the journal\n" +
	"writes behind.\n\n" +
	"--output writes a gzipped tar (default `goobers-diagnostics-<instance>.tar.gz`\n" +
	"in the working directory); --json writes the machine-readable document to\n" +
	"stdout instead. Two collections of the same instance state produce identical\n" +
	"bytes apart from the collection timestamp.\n\n" +
	"Exit codes: 0 = bundle written, 1 = collection failed, 2 = usage error.\n"

const diagnosticsHelp = "Usage: goobers diagnostics <subcommand> [flags] [path]\n\n" +
	"Collect portable, redacted support evidence from this binary and an instance\n" +
	"directory alone — reading Goobers' own source must never be a prerequisite\n" +
	"for reading a Goobers incident.\n\n" +
	"Subcommands:\n" +
	"  bundle  write a redacted diagnostics archive (or --json to stdout)\n\n" +
	"Default path is \".\".\n"

func runDiagnostics(args []string, stdout, stderr io.Writer) int {
	_ = args
	_ = stdout
	helpUsage(stderr, "diagnostics")()
	return 2
}

func runDiagnosticsBundle(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("diagnostics bundle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "diagnostics bundle")
	var (
		runID   = fs.String("run", "", "limit the bundle to one run id")
		pr      = fs.Int("pr", 0, "limit the bundle to runs that touched one pull request")
		maxRuns = fs.Int("max-runs", diagnostics.DefaultMaxRuns, "maximum runs to collect when no run is named")
		output  = fs.String("output", "", "write the archive to this path")
		asJSON  = fs.Bool("json", false, "write the machine-readable document to stdout instead of an archive")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if *maxRuns < 1 {
		pf(stderr, "error: --max-runs must be at least 1\n")
		return 2
	}
	if *runID != "" && *pr > 0 {
		pf(stderr, "error: --run and --pr select different scopes; pass one\n")
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		pf(stderr, "error: resolve instance path: %v\n", err)
		return 2
	}

	bundle, err := diagnosticsCollector().Collect(diagnostics.Options{
		Root:    absRoot,
		Now:     time.Now().UTC(),
		RunID:   *runID,
		PR:      *pr,
		MaxRuns: *maxRuns,
	})
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	if *asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(bundle); err != nil {
			pf(stderr, "error: encode diagnostics: %v\n", err)
			return 1
		}
		return 0
	}

	path := *output
	if strings.TrimSpace(path) == "" {
		path = "goobers-diagnostics-" + filepath.Base(absRoot) + ".tar.gz"
	}
	file, err := os.Create(path)
	if err != nil {
		pf(stderr, "error: create %s: %v\n", path, err)
		return 1
	}
	writeErr := diagnostics.WriteArchive(file, bundle)
	closeErr := file.Close()
	if writeErr != nil {
		pf(stderr, "error: write %s: %v\n", path, writeErr)
		return 1
	}
	if closeErr != nil {
		pf(stderr, "error: close %s: %v\n", path, closeErr)
		return 1
	}
	pf(stdout, "wrote %s (%d run(s), %d credential(s))\n", path, len(bundle.Runs), len(bundle.Credentials))
	if len(bundle.Notes) > 0 {
		pf(stderr, "note: %d source(s) could not be collected; see %s\n", len(bundle.Notes), diagnostics.FileSummary)
	}
	return 0
}

// diagnosticsCollector wires the collector to this binary's own identity and
// to the instance readers that live in package main.
func diagnosticsCollector() diagnostics.Collector {
	return diagnostics.Collector{
		Binary: diagnostics.BinaryInfo{
			Version: version.Version,
			Commit:  version.Commit,
			Date:    version.Date,
			OS:      runtime.GOOS,
			Arch:    runtime.GOARCH,
			Go:      runtime.Version(),
		},
		Contract: diagnosticsContract(),
		Scrubber: journal.NewPatternScrubber(),
		Daemon:   diagnosticsDaemonInfo,
		Instance: diagnosticsInstanceInfo,
		RunDirs:  diagnosticsRunDirs,
	}
}

// diagnosticsContract projects the contract surface the RUNNING binary
// implements. This is the half an operator previously had to read
// providerstage/manifest.go and the journal schema constants for.
func diagnosticsContract() diagnostics.ContractInfo {
	info := diagnostics.ContractInfo{JournalSchemaVersion: journal.CurrentSchemaVersion}
	for _, dsl := range supportmatrix.GetDSL().Versions() {
		info.DSLVersions = append(info.DSLVersions, dsl.Version)
	}
	for _, name := range providerstage.Commands() {
		entry, ok := providerstage.Lookup(name)
		if !ok {
			continue
		}
		command := diagnostics.StageCommandInfo{Command: name, ResultFile: entry.ResultFile}
		for _, use := range entry.Capabilities {
			command.Capabilities = append(command.Capabilities, string(use.Capability))
		}
		sort.Strings(command.Capabilities)
		command.Capabilities = dedupeStrings(command.Capabilities)
		info.StageCommands = append(info.StageCommands, command)
	}
	return info
}

func dedupeStrings(values []string) []string {
	out := values[:0]
	var previous string
	for i, value := range values {
		if i > 0 && value == previous {
			continue
		}
		previous = value
		out = append(out, value)
	}
	return out
}

// diagnosticsRunDirs enumerates individual run directories across every
// gaggle's journal root. Layout.RunDirs returns the ROOTS, not the runs, so a
// caller that hands those straight to a journal reader silently collects
// nothing — the shape this collector's first draft got wrong.
func diagnosticsRunDirs(root string) ([]string, error) {
	roots, err := layoutFor(root).RunDirs()
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, runsDir := range roots {
		entries, err := os.ReadDir(runsDir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", runsDir, err)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				dirs = append(dirs, filepath.Join(runsDir, entry.Name()))
			}
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

func diagnosticsDaemonInfo(root string, now time.Time) (diagnostics.DaemonInfo, error) {
	lockPath := filepath.Join(layoutFor(root).SchedulerDir(), "up.lock")
	info := diagnostics.DaemonInfo{}
	if _, err := os.Stat(lockPath); err == nil {
		info.LockPresent = true
	}
	running, identity, liveness, err := inspectDaemonLiveness(lockPath, now)
	if err != nil {
		return info, err
	}
	info.Running = running
	info.Stale = running && !liveness.Healthy
	// A present-but-unheld lock file's own holderKind (#4833) distinguishes a
	// foreground `goobers run` (expected residue — it acquires and releases
	// the same lock file) from an actual daemon crash. A read/decode failure
	// here leaves LockHolderKind empty rather than failing the whole bundle
	// collection — the summary then falls back to the conservative "possible
	// crash" wording, same as before this field existed.
	if info.LockPresent && !running {
		if state, err := readInstanceLockStatePath(lockPath); err == nil && state != nil {
			info.LockHolderKind = string(state.HolderKind)
		}
	}
	if !liveness.LastTickAt.IsZero() {
		info.LastTickAt = liveness.LastTickAt.UTC().Format(time.RFC3339)
	}
	if identity != nil {
		info.PID = identity.PID
		info.Version = identity.Version
		if !identity.StartedAt.IsZero() {
			info.StartedAt = identity.StartedAt.UTC().Format(time.RFC3339)
		}
	}
	return info, nil
}

// diagnosticsInstanceInfo projects the loaded configuration generation and the
// declared credentials. It reads names and digests; it resolves nothing.
func diagnosticsInstanceInfo(root string) (diagnostics.InstanceInfo, []diagnostics.CredentialPresence, error) {
	layout := layoutFor(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return diagnostics.InstanceInfo{}, nil, fmt.Errorf("load instance.yaml: %w", err)
	}
	info := diagnostics.InstanceInfo{}
	credentials := diagnosticsCredentials(cfg)

	set, report, err := loadConfigDirectory(layout.ConfigDir())
	if err != nil && set == nil {
		return info, credentials, fmt.Errorf("load config directory: %w", err)
	}
	// A config that fails validation is exactly what a bundle must report: the
	// operator's question is often "why is this instance behaving oddly", and
	// "its config does not validate" is the answer. Discarding the report would
	// leave the bundle describing a generation the daemon may be refusing to
	// load at all.
	info.ConfigIssues = diagnosticsConfigIssues(report)
	info.ConfigDigest, info.Gaggles = diagnosticsConfigGeneration(set)
	return info, credentials, nil
}

// diagnosticsConfigIssues projects the validation report's errors and warnings
// as bounded, code-first strings. Codes and file paths, not free prose dumps:
// the bundle names what is wrong and where, and the operator runs `goobers
// validate` for the full text.
func diagnosticsConfigIssues(report *validate.Report) []string {
	if report == nil {
		return nil
	}
	issues := make([]string, 0, len(report.Issues))
	for _, issue := range report.Issues {
		issues = append(issues, fmt.Sprintf("%s %s %s: %s",
			strings.ToUpper(string(issue.Severity)), issue.Code, issue.File, issue.Message))
	}
	sort.Strings(issues)
	return issues
}

// diagnosticsConfigGeneration digests the loaded config tree and projects each
// gaggle's workflow and goober identities.
//
// The digest is over the DEFINITIONS, not the files: two instances whose YAML
// differs only in comments are the same generation, and an operator comparing
// two bundles wants to know whether the thing that RUNS changed.
//
// It is deliberately NOT the compiled Machine.Digest() a run journal pins.
// Computing that one means compiling with the daemon's full option set, which
// includes resolving the agent:model credential for harness model discovery —
// and materializing a secret is the one thing this bundle must never do. So
// the field is named for what it is, a definition digest, and the summary says
// so; a run's own compiled workflowDigest is reported separately, straight
// from its journal, where it was already recorded.
func diagnosticsConfigGeneration(set *instance.ConfigSet) (string, []diagnostics.GaggleInfo) {
	if set == nil {
		return "", nil
	}
	hash := sha256.New()
	byGaggle := map[string]*diagnostics.GaggleInfo{}
	order := make([]string, 0, len(set.Gaggles))
	for _, gaggle := range set.Gaggles {
		if _, seen := byGaggle[gaggle.Name]; !seen {
			byGaggle[gaggle.Name] = &diagnostics.GaggleInfo{Name: gaggle.Name}
			order = append(order, gaggle.Name)
		}
	}
	for _, goober := range set.Goobers {
		if entry, ok := byGaggle[goober.Spec.Gaggle]; ok {
			entry.Goobers = append(entry.Goobers, goober.Name)
		}
	}
	for i := range set.Workflows {
		wf := &set.Workflows[i]
		entry, ok := byGaggle[wf.Spec.Gaggle]
		if !ok {
			continue
		}
		entry.Workflows = append(entry.Workflows, diagnostics.WorkflowInfo{
			Name:             wf.Name,
			DSLVersion:       wf.DSLVersion,
			DefinitionDigest: definitionDigest(wf),
		})
	}
	sort.Strings(order)
	gaggles := make([]diagnostics.GaggleInfo, 0, len(order))
	for _, name := range order {
		entry := byGaggle[name]
		sort.Strings(entry.Goobers)
		sort.Slice(entry.Workflows, func(i, j int) bool { return entry.Workflows[i].Name < entry.Workflows[j].Name })
		// hash is a sha256 writer: its Write never returns an error, and the
		// digest is verified end to end by the reproducibility test.
		_, _ = fmt.Fprintf(hash, "gaggle:%s\n", entry.Name)
		for _, goober := range entry.Goobers {
			_, _ = fmt.Fprintf(hash, "goober:%s\n", goober)
		}
		for _, wf := range entry.Workflows {
			_, _ = fmt.Fprintf(hash, "workflow:%s:%s:%s\n", wf.Name, wf.DSLVersion, wf.DefinitionDigest)
		}
		gaggles = append(gaggles, *entry)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), gaggles
}

// definitionDigest hashes a workflow's authored identity through the canonical
// JSON encoding the wire contract already uses, so the value is stable across
// hosts and YAML formatting.
func definitionDigest(wf *apiv1.Workflow) string {
	payload, err := json.Marshal(struct {
		Name       string             `json:"name"`
		DSLVersion string             `json:"dslVersion"`
		Spec       apiv1.WorkflowSpec `json:"spec"`
	}{Name: wf.Name, DSLVersion: wf.DSLVersion, Spec: wf.Spec})
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func diagnosticsCredentials(cfg *instance.Config) []diagnostics.CredentialPresence {
	if cfg == nil {
		return nil
	}
	out := make([]diagnostics.CredentialPresence, 0, len(cfg.Credentials)+len(cfg.Repos))
	// Repository tokens first: an instance that declares no explicit
	// credentials: block still HAS credentials, one per repo, and a bundle
	// that reported none would answer "were the credentials there" with
	// silence.
	for _, repo := range cfg.Repos {
		kind, source := diagnosticsTokenSource(repo.Token)
		name := fmt.Sprintf("repo:%s/%s", repo.Owner, repo.Name)
		if repo.Project != "" {
			name = fmt.Sprintf("repo:%s/%s/%s", repo.Owner, repo.Project, repo.Name)
		}
		out = append(out, diagnostics.CredentialPresenceFor(name, kind, source, os.LookupEnv))
	}
	for _, grant := range cfg.Credentials {
		name := grant.Capability
		if name == "" {
			name = "mcp:" + grant.MCP
		}
		kind, source := diagnosticsTokenSource(grant.Token)
		out = append(out, diagnostics.CredentialPresenceFor(name, kind, source, os.LookupEnv))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Capability < out[j].Capability })
	return out
}

// diagnosticsTokenSource names a token reference's source without reading it.
func diagnosticsTokenSource(ref instance.TokenRef) (kind, name string) {
	switch {
	case ref.Env != "":
		return "env", ref.Env
	case ref.File != "":
		return "file", ref.File
	case ref.Keychain != "":
		return "keychain", ref.Keychain
	case ref.Store != "":
		return "store", ref.Store
	case ref.GitHubCLI != nil:
		return "githubCLI", ref.GitHubCLI.Hostname + "/" + ref.GitHubCLI.User
	default:
		return "unset", ""
	}
}
