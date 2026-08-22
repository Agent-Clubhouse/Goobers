package providerstage

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/supportmatrix"
)

// These tests pin the #3504 contract (dsl-3.0.md D7/§8): the manifest's
// version linkage must make a change gated to a later DSL version invisible
// to earlier interpreters, while leaving today's all-baseline table resolving
// byte-identically for every shipped version.

// TestShippedVersionViewsResolveBaselineIdentically proves requirement (2) of
// #3504: with no version gates live, the 1.4 and 2.0 views resolve every
// entry exactly as the unversioned table did — byte-identical against the
// committed snapshot. This is deliberately a tripwire on "no live gates yet":
// the first real 3.0-gated change (#3505 era) breaks it on purpose, and its
// author replaces the expectation alongside the snapshot + patch-log move.
func TestShippedVersionViewsResolveBaselineIdentically(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "manifest_snapshot.json"))
	if err != nil {
		t.Fatalf("read committed snapshot: %v", err)
	}
	for _, version := range shippedDSLVersions {
		t.Run("DSL "+version, func(t *testing.T) {
			resolved := make(map[string]Command, len(commands))
			for _, name := range Commands() {
				entry, ok := lookupIn(commands, version, name)
				if !ok {
					t.Fatalf("command %q is invisible at DSL %s — the shipped table must stay all-baseline until a 3.0 change lands deliberately", name, version)
				}
				resolved[name] = entry
			}
			got, err := renderManifestSnapshot(resolved)
			if err != nil {
				t.Fatalf("render resolved view: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("the DSL %s view resolves differently from the committed baseline snapshot — a version gate is live on the shipped table", version)
			}
		})
	}
}

// TestVersionGatedChangesInvisibleToEarlierViews proves requirement (1)/(3)
// of #3504 on a synthetic 3.0-era change set: the exact ed11ae81 class (a
// requirement swap), a derived-requirement addition, a new command, and a
// retired command — all gated to DSL 3.0. The 1.4 and 2.0 views of the
// changed table must be byte-identical to the views of the unchanged table
// (a 2.0 compile cannot see any of it), while a 3.0 view sees all of it.
// The test drives the same requiredCapabilitiesIn/mutatesClaimLedgerIn/
// lookupIn resolution the interpreters' views use.
func TestVersionGatedChangesInvisibleToEarlierViews(t *testing.T) {
	gated := cloneTable(commands)

	// The ed11ae81 shape, gated this time: backlog-dedupe's requirement
	// swaps read -> write at 3.0. Two rows: the old use closes at 3.0, the
	// replacement opens at 3.0.
	dedupe := gated["backlog-dedupe"]
	dedupe.Capabilities = []CapabilityUse{
		{Capability: capability.GitHubIssuesRead, Consequence: dedupe.Capabilities[0].Consequence, exact: dedupe.Capabilities[0].exact, untilDSL: "3.0"},
		{Capability: capability.GitHubIssuesWrite, Consequence: "synthetic 3.0 requirement swap", sinceDSL: "3.0"},
	}
	gated["backlog-dedupe"] = dedupe

	// A derived-requirement addition (the routine Goobernetes-era change):
	// push-branch gains a second requirement only from 3.0 on.
	push := gated["push-branch"]
	push.Capabilities = append(push.Capabilities,
		CapabilityUse{Capability: capability.GitHubPRWrite, Consequence: "synthetic 3.0 derived requirement", sinceDSL: "3.0"})
	gated["push-branch"] = push

	// A command that exists only from 3.0, and one retired at 3.0.
	gated["runs-on-probe"] = Command{
		ResultFile: "runs-on-probe.json",
		Capabilities: []CapabilityUse{
			{Capability: capability.GitHubIssuesRead, Consequence: "synthetic 3.0-only command"},
		},
		sinceDSL: "3.0",
	}
	retired := gated["pr-select"]
	retired.untilDSL = "3.0"
	gated["pr-select"] = retired

	for _, version := range shippedDSLVersions {
		t.Run("DSL "+version+" cannot see the gated changes", func(t *testing.T) {
			if !bytes.Equal(resolveView(t, gated, version), resolveView(t, commands, version)) {
				t.Fatalf("the DSL %s view of the 3.0-gated table differs from the view of the live table — a gated change leaked into an earlier interpreter", version)
			}
			if got := capabilitiesOf(requiredCapabilitiesIn(gated, version, "backlog-dedupe", nil)); !slices.Equal(got, []capability.Capability{capability.GitHubIssuesRead}) {
				t.Fatalf("backlog-dedupe at DSL %s requires %q, want the pre-swap [github:issues:read]", version, got)
			}
			if !mutatesClaimLedgerIn(gated, version, "pr-select", nil) {
				t.Fatalf("pr-select at DSL %s lost its claim-ledger bit before its retirement version", version)
			}
			if _, ok := lookupIn(gated, version, "runs-on-probe"); ok {
				t.Fatalf("the 3.0-only command is visible at DSL %s", version)
			}
		})
	}

	t.Run("DSL 3.0 sees all of it", func(t *testing.T) {
		if got := capabilitiesOf(requiredCapabilitiesIn(gated, "3.0", "backlog-dedupe", nil)); !slices.Equal(got, []capability.Capability{capability.GitHubIssuesWrite}) {
			t.Fatalf("backlog-dedupe at DSL 3.0 requires %q, want the swapped [github:issues:write]", got)
		}
		if got := capabilitiesOf(requiredCapabilitiesIn(gated, "3.0", "push-branch", nil)); !slices.Equal(got, []capability.Capability{capability.RepoPush, capability.GitHubPRWrite}) {
			t.Fatalf("push-branch at DSL 3.0 requires %q, want the baseline plus the 3.0 addition", got)
		}
		if _, ok := lookupIn(gated, "3.0", "runs-on-probe"); !ok {
			t.Fatal("the 3.0-only command is invisible at DSL 3.0")
		}
		if _, ok := lookupIn(gated, "3.0", "pr-select"); ok {
			t.Fatal("the command retired at 3.0 is still visible at DSL 3.0")
		}
		if mutatesClaimLedgerIn(gated, "3.0", "pr-select", nil) {
			t.Fatal("the command retired at 3.0 still reports claim-ledger mutation at DSL 3.0")
		}
	})
}

// TestVersionWindowsFailClosed pins the malformed-version posture the package
// comment promises: an unparseable bound hides the gated element, and an
// unparseable view version sees only baseline elements.
func TestVersionWindowsFailClosed(t *testing.T) {
	table := map[string]Command{
		"baseline": {Capabilities: []CapabilityUse{
			{Capability: capability.GitHubIssuesRead, Consequence: "baseline"},
			{Capability: capability.GitHubIssuesWrite, Consequence: "gated", sinceDSL: "3.0"},
		}},
		"gated":   {sinceDSL: "3.0"},
		"badgate": {sinceDSL: "not-a-version"},
	}
	if _, ok := lookupIn(table, "not-a-version", "baseline"); !ok {
		t.Fatal("an unparseable view version must still see baseline entries")
	}
	if got := capabilitiesOf(requiredCapabilitiesIn(table, "not-a-version", "baseline", nil)); !slices.Equal(got, []capability.Capability{capability.GitHubIssuesRead}) {
		t.Fatalf("an unparseable view version resolves %q, want only the baseline use", got)
	}
	if _, ok := lookupIn(table, "not-a-version", "gated"); ok {
		t.Fatal("an unparseable view version must not see gated entries")
	}
	if _, ok := lookupIn(table, "3.0", "badgate"); ok {
		t.Fatal("an unparseable window bound must hide the gated entry")
	}
}

// TestLiveTableVersionWindowsAreWellFormed guards future gates: every bound
// on the live table must parse as a DSL version, and a closed window must be
// non-empty — a malformed bound would silently fail closed at resolution.
func TestLiveTableVersionWindowsAreWellFormed(t *testing.T) {
	checkWindow := func(t *testing.T, owner, since, until string) {
		t.Helper()
		for _, bound := range []string{since, until} {
			if bound == "" {
				continue
			}
			if _, ok := supportmatrix.CompareDSLVersions(bound, bound); !ok {
				t.Errorf("%s declares version bound %q that does not parse as <major>.<minor>", owner, bound)
			}
		}
		if since != "" && until != "" {
			if order, ok := supportmatrix.CompareDSLVersions(since, until); ok && order >= 0 {
				t.Errorf("%s declares an empty version window [%s, %s)", owner, since, until)
			}
		}
	}
	for name, entry := range commands {
		checkWindow(t, "command "+name, entry.sinceDSL, entry.untilDSL)
		for _, use := range entry.Capabilities {
			checkWindow(t, "command "+name+" capability "+string(use.Capability), use.sinceDSL, use.untilDSL)
		}
	}
}

func cloneTable(table map[string]Command) map[string]Command {
	out := make(map[string]Command, len(table))
	for name, entry := range table {
		entry.Capabilities = append([]CapabilityUse(nil), entry.Capabilities...)
		entry.claimMutationFlags = append([]string(nil), entry.claimMutationFlags...)
		out[name] = entry
	}
	return out
}

// resolveView renders table as resolved at version, so two tables can be
// compared for byte-identical resolution.
func resolveView(t *testing.T, table map[string]Command, version string) []byte {
	t.Helper()
	resolved := make(map[string]Command, len(table))
	for name := range table {
		if entry, ok := lookupIn(table, version, name); ok {
			resolved[name] = entry
		}
	}
	raw, err := renderManifestSnapshot(resolved)
	if err != nil {
		t.Fatalf("render resolved view at DSL %s: %v", version, err)
	}
	return raw
}

func capabilitiesOf(uses []CapabilityUse) []capability.Capability {
	got := make([]capability.Capability, 0, len(uses))
	for _, use := range uses {
		got = append(got, use.Capability)
	}
	return got
}
