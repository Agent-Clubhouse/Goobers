// Command validate lints a Goobers config-as-code directory against the
// canonical schemas + cross-object reference rules, or validates a single
// runtime envelope file. Exit code 0 means valid; 1 means validation errors;
// 2 means a usage/IO error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/goobers/goobers/api/validate"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// pf/pln are thin print helpers that discard the write error — these are
// terminal CLI writes to stdout/stderr where a failed write is not actionable,
// and they keep call sites free of repeated `_, _ =`.
func pf(w io.Writer, format string, a ...interface{}) { _, _ = fmt.Fprintf(w, format, a...) }
func pln(w io.Writer, s string)                       { _, _ = fmt.Fprintln(w, s) }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit the report as JSON")
	envelope := fs.String("envelope", "", "validate a single envelope file instead of a config dir: invocation|result|verdict")
	fs.Usage = func() {
		pf(stderr, `validate — lint Goobers config-as-code

Usage:
  validate [--json] <config-dir>
  validate --envelope <invocation|result|verdict> [--json] <file.json|file.yaml>

Exit codes: 0 = valid, 1 = validation errors, 2 = usage/IO error
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	target := fs.Arg(0)

	v, err := validate.New()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	if *envelope != "" {
		return runEnvelope(v, *envelope, target, *asJSON, stdout, stderr)
	}
	return runDir(v, target, *asJSON, stdout, stderr)
}

func runDir(v *validate.Validator, dir string, asJSON bool, stdout, stderr io.Writer) int {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		pf(stderr, "error: %q is not a directory\n", dir)
		return 2
	}
	report, err := v.ValidateDir(dir)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if asJSON {
		writeJSON(stdout, report)
	} else {
		for _, issue := range report.Issues {
			pln(stdout, issue.String())
		}
		errs := 0
		for _, i := range report.Issues {
			if i.Severity == validate.Error {
				errs++
			}
		}
		pf(stdout, "\nchecked %d object(s) across %d file(s): %d error(s), %d issue(s) total\n",
			report.Objects, report.Files, errs, len(report.Issues))
	}
	if report.HasErrors() {
		return 1
	}
	return 0
}

func runEnvelope(v *validate.Validator, name, file string, asJSON bool, stdout, stderr io.Writer) int {
	raw, err := os.ReadFile(file)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	// Accept YAML or JSON; envelopes are JSON on the wire but config-style YAML
	// is convenient for fixtures.
	jsonBytes, err := toJSON(file, raw)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if err := v.ValidateEnvelope(name, jsonBytes); err != nil {
		if asJSON {
			writeJSON(stdout, map[string]string{"valid": "false", "error": err.Error()})
		} else {
			pf(stdout, "INVALID %s envelope %s:\n%v\n", name, file, err)
		}
		return 1
	}
	if asJSON {
		pln(stdout, `{"valid":true}`)
	} else {
		pf(stdout, "OK: %s is a valid %s envelope\n", file, name)
	}
	return 0
}

func writeJSON(w io.Writer, v interface{}) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
