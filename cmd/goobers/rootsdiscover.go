package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"time"

	"github.com/goobers/goobers/internal/instance"
)

const rootsDiscoverHelp = "Usage: goobers roots discover [--json] [paths...]\n\n" +
	"Search supplied directories, or the current directory and user home by default.\n" +
	"Read-only: legacy roots are not adopted. Search is limited to 20,000 directory\n" +
	"entries, depth 8, and 10 seconds; incomplete scans are reported explicitly.\n" +
	"Child symlinks, .git, node_modules, .cache, Library, vendor, and instance interiors\n" +
	"are not searched. Supply other storage locations explicitly. Read-model modification\n" +
	"time is only file freshness, never proof of a running daemon. Exit codes: 0 = complete\n" +
	"within the stated search scope, 1 = partial, 2 = error.\n"

type discoveredRoot struct {
	statusRootIdentity
	ReadModelModifiedAt *time.Time `json:"readModelModifiedAt,omitempty"`
	Gaggles             []string   `json:"gaggles,omitempty"`
	Repositories        []string   `json:"repositories,omitempty"`
	ScopeProblem        string     `json:"scopeProblem,omitempty"`
}

type rootsDiscoveryOutput struct {
	SearchBases []string         `json:"searchBases"`
	Partial     bool             `json:"partial"`
	Roots       []discoveredRoot `json:"roots"`
	Warnings    []string         `json:"warnings"`
}

func runRootsDiscover(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("roots discover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "roots discover")
	asJSON := fs.Bool("json", false, "emit structured discovery")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	bases := fs.Args()
	if len(bases) == 0 {
		bases = []string{"."}
		if home, err := os.UserHomeDir(); err == nil {
			bases = append(bases, home)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	found, err := instance.DiscoverRoots(ctx, bases, 20000, 8)
	if err != nil {
		found.Partial = true
	}
	output := rootsDiscoveryOutput{SearchBases: bases, Partial: found.Partial, Roots: []discoveredRoot{}}
	for _, path := range found.Paths {
		if ctx.Err() != nil {
			output.Partial = true
			break
		}
		layout := instance.NewLayout(path)
		root := discoveredRoot{statusRootIdentity: inspectStatusRoot(layout, time.Now())}
		if info, err := os.Stat(layout.ReadDB()); err == nil && info.Mode().IsRegular() {
			modified := info.ModTime().UTC()
			root.ReadModelModifiedAt = &modified
		}
		output.Roots = append(output.Roots, root)
	}
	inspectDiscoveryScopes(ctx, &output)
	if *asJSON {
		if err := json.NewEncoder(stdout).Encode(output); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
	} else {
		writeRootsDiscovery(stdout, output)
	}
	if output.Partial {
		return 1
	}
	return 0
}

func writeRootsDiscovery(w io.Writer, output rootsDiscoveryOutput) {
	pf(w, "Root discovery search bases: %q\n", output.SearchBases)
	for _, root := range output.Roots {
		writeStatusRoot(w, root.statusRootIdentity)
		if root.ScopeProblem != "" {
			pf(w, "  Scope warning: %s\n", root.ScopeProblem)
		}
		if root.ReadModelModifiedAt != nil {
			pf(w, "  Read-model file modified: %s (not daemon liveness)\n", root.ReadModelModifiedAt.Format(time.RFC3339Nano))
		}
	}
	for _, warning := range output.Warnings {
		pf(w, "Warning: %s\n", warning)
	}
	if output.Partial {
		pln(w, "Warning: discovery is partial; absence from this list does not establish absence of another root.")
	}
}
