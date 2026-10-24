// Command migrationorder is a pre-merge gate: every cloud migration a change
// adds must be numbered above the highest migration on its base.
//
// The migrate runner applies every catalog version missing from
// control.schema_migration, in version order, whatever it already applied. A
// migration numbered below one a database already has would therefore run out
// of order, after the migrations that were written to follow it. Comparing
// against the base (not the merge-base) makes a stale branch fail as soon as
// its base gains a higher migration, so it is renumbered before it can merge.
//
//	go run ./test/migrationorder -base-ref <base> -head-ref <head>
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// migrationsDir is where the cloud catalog lives (internal/cloud/migrate).
const migrationsDir = "internal/cloud/migrate/migrations"

var migrationName = regexp.MustCompile(`^([0-9]+)_[a-z0-9_]+\.(expand|contract)\.sql$`)

type gitFunc func(args ...string) ([]byte, error)

func main() {
	os.Exit(run(os.Args[1:], func(args ...string) ([]byte, error) {
		return exec.Command("git", args...).Output()
	}, os.Stdout, os.Stderr))
}

func run(args []string, git gitFunc, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("migrationorder", flag.ContinueOnError)
	flags.SetOutput(stderr)
	baseRef := flags.String("base-ref", "", "the target branch revision the change merges into")
	headRef := flags.String("head-ref", "HEAD", "the change's revision")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*baseRef) == "" || flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "usage: migrationorder -base-ref <base> [-head-ref <head>]")
		return 2
	}
	head := *headRef
	if strings.TrimSpace(head) == "" {
		head = "HEAD"
	}
	violations, baseMax, err := check(git, *baseRef, head)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "migrationorder: %v\n", err)
		return 2
	}
	if len(violations) > 0 {
		for _, name := range violations {
			_, _ = fmt.Fprintf(stderr, "migrationorder: %s/%s is numbered at or below %04d, the highest migration on the base; renumber it above %04d (and above any number an open change has reserved). The runner applies any version a database lacks, so it would run out of order after migrations written to follow it.\n", migrationsDir, name, baseMax, baseMax)
		}
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "migrationorder: every added migration is numbered above the base's %04d\n", baseMax)
	return 0
}

// check returns the migrations head adds that are not numbered above the
// base's highest version, and that version.
func check(git gitFunc, baseRef, headRef string) ([]string, int, error) {
	base, err := migrations(git, baseRef)
	if err != nil {
		return nil, 0, err
	}
	head, err := migrations(git, headRef)
	if err != nil {
		return nil, 0, err
	}
	baseMax := 0
	for _, version := range base {
		baseMax = max(baseMax, version)
	}
	var violations []string
	for name, version := range head {
		if _, existed := base[name]; existed {
			continue
		}
		if version <= baseMax {
			violations = append(violations, name)
		}
	}
	slices.Sort(violations)
	return violations, baseMax, nil
}

// migrations maps each migration file at ref to its version.
func migrations(git gitFunc, ref string) (map[string]int, error) {
	output, err := git("ls-tree", "--name-only", "--end-of-options", ref+"^{commit}", migrationsDir+"/")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return nil, fmt.Errorf("list migrations at %s: %s", ref, strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, fmt.Errorf("list migrations at %s: %w", ref, err)
	}
	found := make(map[string]int)
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		name := path.Base(strings.TrimSpace(line))
		if name == "" || name == "." || !strings.HasSuffix(name, ".sql") {
			continue
		}
		match := migrationName.FindStringSubmatch(name)
		if match == nil {
			return nil, fmt.Errorf("migration file %s at %s is not named NNNN_name.(expand|contract).sql", name, ref)
		}
		version, err := strconv.Atoi(match[1])
		if err != nil {
			return nil, fmt.Errorf("migration file %s at %s: %w", name, ref, err)
		}
		found[name] = version
	}
	return found, nil
}
