package main

import "io"

// lintHelp is the `goobers lint` help body. lint is the memorable, canonical
// entry point CI and local dev share for config validation (#439/AUTH-2): it is
// a thin alias over the exact `goobers validate` engine — same flags, same
// checks, same exit codes — so an automated gate and a developer at a terminal
// can never get contradictory verdicts from two drifting validators (the #252
// footgun). Anything `validate` accepts, `lint` accepts identically.
const lintHelp = "Usage: goobers lint [--check-harness] [--check-repos] [--source-tree] [path]\n\n" +
	"Lint an instance's instance.yaml and config/ directory (default path\n" +
	"\".\") against the single authoritative validation engine. This is an\n" +
	"alias for `goobers validate`: identical flags, identical checks, and\n" +
	"identical exit codes, so CI and local development share one validation\n" +
	"path instead of drifting between ad-hoc checks. --source-tree lints a\n" +
	"checked-in config source tree using instance.yaml.example and the path\n" +
	"itself as config/. --check-harness additionally preflights every agent\n" +
	"harness referenced by a goober (GBO-011). --check-repos resolves each\n" +
	"target repository's token and verifies authenticated git access. Exit\n" +
	"codes: 0 = clean, 1 = findings, 2 = usage/IO error.\n"

// runLint implements `goobers lint` (#439). It delegates to the shared
// validation engine under the "lint" verb identity, guaranteeing there is one
// authoritative validation path rather than a separate lint checker that could
// diverge from `validate` — exactly the drift #252 eliminated between the old
// cmd/validate and `goobers validate`.
func runLint(args []string, stdout, stderr io.Writer) int {
	return runValidateAs("lint", args, stdout, stderr)
}
