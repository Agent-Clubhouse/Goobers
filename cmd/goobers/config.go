package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/configdiff"
	"github.com/goobers/goobers/internal/instance"
)

const configHelp = "Usage: goobers config <subcommand> [flags] [path]\n\n" +
	"Inspect instance configuration and compare workflow definitions.\n\n" +
	"Subcommands:\n" +
	"  show   render the effective instance config (secrets redacted)\n" +
	"  diff   compare active workflows with the shipped canonical workflows\n\n" +
	"Run `goobers config show -h` or `goobers config diff -h` for details.\n" +
	"Default path is \".\".\n"

const configShowHelp = "Usage: goobers config show [--json] [path]\n\n" +
	"Render the effective instance configuration loaded from <path>/instance.yaml\n" +
	"(repos, API/webhook bind config, run conditions, credential grants, timezone),\n" +
	"as YAML by default or as JSON with --json.\n\n" +
	"Secrets are redacted by construction: a token reference is only ever a\n" +
	"locator — the env var name or file path the secret is read from at runtime\n" +
	"(CFG-009/SEC-010 forbid inline values) — so `config show` prints where each\n" +
	"credential comes from and never reads or prints the secret value itself.\n\n" +
	"Exit codes: 0 = OK, 1 = load/render error, 2 = usage error or not an\n" +
	"instance root.\n"

const configDiffHelp = "Usage: goobers config diff [--against <canonical-root>] [instance-root]\n\n" +
	"Compare the active workflows under <instance-root>/config with a canonical\n" +
	"config source tree. The canonical root defaults to ./selfhost; use --against\n" +
	"when running outside the Goobers source checkout or comparing another set.\n\n" +
	"Schedule, maxConcurrentRuns, maxRunsPerHour, maxRunsPerDay, maxOpenPRs,\n" +
	"and trigger presence (enablement) are operational tuning: they are printed\n" +
	"as INFO and do not fail the command. Every other workflow difference is\n" +
	"structural drift, printed as ERROR with active and canonical values.\n\n" +
	"Exit codes: 0 = structurally identical (informational tuning is allowed),\n" +
	"1 = structural drift or invalid config, 2 = usage/IO error.\n"

// runConfig is the `config` group dispatcher: it only handles the bare/`-h`
// invocation, since real work lives in the show and diff subcommands.
func runConfig(args []string, stdout, stderr io.Writer) int {
	usage := func(w io.Writer) { pf(w, "%s", configHelp) }
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		usage(stdout)
		return 0
	}
	if len(args) > 0 {
		pf(stderr, "error: unknown config command %q\n", args[0])
	}
	usage(stderr)
	return 2
}

// runConfigShow renders the effective instance config with secrets redacted.
func runConfigShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "render the config as JSON instead of YAML")
	fs.Usage = helpUsage(stderr, "config show")
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

	l := instance.NewLayout(root)
	configFile := l.ConfigFile()
	if _, err := os.Stat(configFile); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", configFile)
		return 2
	}
	cfg, err := instance.LoadConfig(configFile)
	if err != nil {
		pf(stderr, "error: load config: %v\n", err)
		return 1
	}

	// The loaded Config is structurally secret-free: every credential is a
	// TokenRef holding only a locator (env var name or file path), and inline
	// token values are rejected at load (config.Validate). So rendering the
	// config as-is exposes where each secret is sourced from without ever
	// reading or printing the secret value — that is the redaction guarantee.
	data, err := json.Marshal(cfg)
	if err != nil {
		pf(stderr, "error: encode config: %v\n", err)
		return 1
	}
	if *asJSON {
		var indented bytes.Buffer
		if err := json.Indent(&indented, data, "", "  "); err != nil {
			pf(stderr, "error: format config: %v\n", err)
			return 1
		}
		pf(stdout, "%s\n", indented.String())
		return 0
	}
	yamlData, err := yaml.JSONToYAML(data)
	if err != nil {
		pf(stderr, "error: render config: %v\n", err)
		return 1
	}
	pf(stdout, "%s", yamlData)
	return 0
}

func runConfigDiff(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("config diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	against := fs.String("against", "selfhost", "canonical config source root")
	fs.Usage = helpUsage(stderr, "config diff")
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

	layout := instance.NewLayout(root)
	if _, err := os.Stat(layout.ConfigFile()); err != nil {
		pf(stderr, "error: %s not found (not an instance root)\n", layout.ConfigFile())
		return 2
	}
	canonicalDir := canonicalConfigDir(*against)

	activeSet, activeReport, err := instance.LoadConfigDir(layout.ConfigDir())
	if err != nil {
		return printConfigDiffLoadError("active", layout.ConfigDir(), activeReport, err, stdout, stderr)
	}
	canonicalSet, canonicalReport, err := instance.LoadConfigDir(canonicalDir)
	if err != nil {
		return printConfigDiffLoadError("canonical", canonicalDir, canonicalReport, err, stdout, stderr)
	}

	differences, err := configdiff.Compare(activeSet.Workflows, canonicalSet.Workflows)
	if err != nil {
		pf(stderr, "error: compare workflows: %v\n", err)
		return 2
	}

	var informational, structural int
	for _, difference := range differences {
		level := "INFO"
		if difference.Severity == configdiff.Error {
			level = "ERROR"
			structural++
		} else {
			informational++
		}
		pf(stdout, "%s workflow=%q", level, difference.Workflow)
		if difference.SubjectKind != "" {
			pf(stdout, " %s=%q", difference.SubjectKind, difference.Subject)
		}
		pf(stdout, " field=%q active=%s canonical=%s\n",
			difference.Field, difference.Active, difference.Canonical)
	}
	if structural > 0 {
		pf(stdout, "DRIFT: %d structural difference(s), %d informational difference(s)\n", structural, informational)
		return 1
	}
	pf(stdout, "OK: workflow structure matches canonical (%d informational difference(s))\n", informational)
	return 0
}

func canonicalConfigDir(root string) string {
	if _, err := os.Stat(filepath.Join(root, instance.ConfigFileName)); err == nil {
		return instance.NewLayout(root).ConfigDir()
	}
	return root
}

func printConfigDiffLoadError(label, dir string, report *validate.Report, err error, stdout, stderr io.Writer) int {
	if errors.Is(err, instance.ErrInvalidConfig) {
		if report != nil {
			printValidationIssues(stdout, report)
		}
		pf(stdout, "INVALID %s config %s\n", label, dir)
		return 1
	}
	pf(stderr, "error: load %s config %s: %v\n", label, dir, err)
	return 2
}
