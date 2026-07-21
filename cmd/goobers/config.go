package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"os"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/instance"
)

const configHelp = "Usage: goobers config <subcommand> [flags] [path]\n\n" +
	"Inspect the instance's operational configuration (instance.yaml).\n\n" +
	"Subcommands:\n" +
	"  show   render the effective instance config (secrets redacted)\n\n" +
	"Run `goobers config show -h` for details. Default path is \".\".\n"

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

// runConfig is the `config` group dispatcher: it only handles the bare/`-h`
// invocation, since real work lives in subcommands (currently just `show`).
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
