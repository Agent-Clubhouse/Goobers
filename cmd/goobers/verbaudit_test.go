package main

import (
	"strings"
	"testing"
)

// auditNode is one command in the registry tree, with the data the verb audit
// needs to reason about it.
type auditNode struct {
	id        string // canonical invocation-path id, e.g. "claims list"
	depth     int
	cmd       cliCommand
	hasSynSub bool // this node or any descendant carries a top-level synopsis
}

// collectAuditNodes walks the registry and returns every command, skipping only
// truly-internal entrypoints (`__`/`_`-prefixed workers and the `help` alias
// that IS the usage mechanism). The `version`/`--version` alias is included: it
// is a real, documented command even though its first name is a flag.
func collectAuditNodes(commands []cliCommand, prefix []string) []auditNode {
	var out []auditNode
	for _, command := range commands {
		if len(command.names) == 0 {
			continue
		}
		id := canonicalID(command, prefix)
		if isInternalCommand(id) {
			continue
		}
		path := append(append([]string{}, prefix...), command.names[0])
		children := collectAuditNodes(command.subcommands, path)
		node := auditNode{id: id, depth: len(prefix), cmd: command, hasSynSub: command.synopsis != ""}
		for _, c := range children {
			if c.cmd.synopsis != "" || c.hasSynSub {
				node.hasSynSub = true
			}
		}
		out = append(out, node)
		out = append(out, children...)
	}
	return out
}

// canonicalID is the space-joined invocation path a command is keyed by in
// synopsisByID / commandHelp. Action-registered commands already carry the full
// path in their action ID (set at construction); groups have no action, so use
// their name joined onto the prefix.
func canonicalID(command cliCommand, prefix []string) string {
	if command.actionRegistered {
		return string(command.action.ID)
	}
	return strings.Join(append(append([]string{}, prefix...), command.names[0]), " ")
}

func isInternalCommand(id string) bool {
	return strings.HasPrefix(id, "_") || id == "help"
}

// TestCLIVerbCoverage is the #1098 verb audit: it cross-checks the command
// registry against the two documented surfaces it must stay in sync with — the
// per-command help metadata and the top-level usage() synopsis map — so an
// orphaned (registered-but-undocumented) or stale (documented-but-unregistered)
// verb fails CI instead of shipping silently.
func TestCLIVerbCoverage(t *testing.T) {
	nodes := collectAuditNodes(cliCommands, nil)
	if len(nodes) == 0 {
		t.Fatal("collected no commands from the registry")
	}

	// A. Every runnable command describes itself (one-line `short`). This is
	// what the generated man pages / CLI reference (CLI-2) will render from.
	for _, n := range nodes {
		if n.cmd.actionRegistered && strings.TrimSpace(n.cmd.short) == "" {
			t.Errorf("command %q has no short help; add .withHelp(short, long)", n.id)
		}
	}

	// B. Every top-level command is discoverable in bare `goobers` usage: it
	// either carries its own synopsis, or is a group whose subcommands do. This
	// is the orphaned-command guard (e.g. elect-lander was registered but
	// missing from usage()).
	for _, n := range nodes {
		if n.depth != 0 {
			continue
		}
		if !n.hasSynSub {
			t.Errorf("top-level command %q is absent from usage(): give it .withSynopsis(synopsisByID[%q]) (and a clisynopsis.go entry), or a subcommand that has one", n.id, n.id)
		}
	}

	// C. No stale synopsis: every clisynopsis.go entry maps to a real command
	// path in the registry (a removed/renamed command must not leave a dangling
	// usage line).
	ids := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		ids[n.id] = true
	}
	for key := range synopsisByID {
		if !ids[key] {
			t.Errorf("synopsisByID has entry %q with no matching command in the registry (stale — remove it or fix the id)", key)
		}
	}
}

// TestCLIVerbCoverageCatchesOrphan is a guard on the audit itself: a top-level
// command with neither a synopsis nor a documented subcommand must be flagged,
// so the audit can't silently rot into a no-op.
func TestCLIVerbCoverageCatchesOrphan(t *testing.T) {
	orphan := cliCommand{names: []string{"phantom-verb"}} // no synopsis, no subcommands
	nodes := collectAuditNodes([]cliCommand{orphan}, nil)
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].id != "phantom-verb" {
		t.Fatalf("id = %q, want %q", nodes[0].id, "phantom-verb")
	}
	if nodes[0].hasSynSub {
		t.Fatal("an undocumented top-level command should not be considered discoverable")
	}
}
