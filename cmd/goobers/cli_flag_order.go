package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

func parseFlagsBeforePath(fs *flag.FlagSet, args []string, stderr io.Writer) bool {
	if err := fs.Parse(args); err != nil {
		return false
	}
	if rejectFlagAfterPath(fs, args, stderr) {
		fs.Usage()
		return false
	}
	return true
}

// rejectFlagAfterPath makes Go's flag-first parsing rule explicit. FlagSet
// stops parsing at the first positional argument; without this check, a later
// flag is silently retained as another positional and callers see only generic
// usage output. An explicit -- keeps the remaining arguments positional.
func rejectFlagAfterPath(fs *flag.FlagSet, args []string, stderr io.Writer) bool {
	if len(fs.Args()) < 2 || flagParsingEndedExplicitly(fs, args) {
		return false
	}
	path := fs.Arg(0)
	for _, arg := range fs.Args()[1:] {
		if arg == "--" {
			return false
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			_, _ = fmt.Fprintf(stderr,
				"error: flags must precede the path argument (got %q after %q)\n",
				arg, path)
			return true
		}
	}
	return false
}

// flagParsingEndedExplicitly distinguishes `command -- -literal-path` from a
// parser stopped by an ordinary positional. It mirrors flag.FlagSet's small
// amount of argument-shape logic only until the first positional or separator;
// Parse has already succeeded, so every flag encountered here is registered.
func flagParsingEndedExplicitly(fs *flag.FlagSet, args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return true
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			return false
		}
		nameAndValue := strings.TrimPrefix(arg, "-")
		nameAndValue = strings.TrimPrefix(nameAndValue, "-")
		name, _, hasValue := strings.Cut(nameAndValue, "=")
		registered := fs.Lookup(name)
		if hasValue || isBooleanCLIFlag(registered) {
			continue
		}
		// A non-boolean flag consumes its following argument, even when that
		// value is "--" or begins with a dash.
		i++
	}
	return false
}

func isBooleanCLIFlag(registered *flag.Flag) bool {
	if registered == nil {
		return false
	}
	boolean, ok := registered.Value.(interface{ IsBoolFlag() bool })
	return ok && boolean.IsBoolFlag()
}
