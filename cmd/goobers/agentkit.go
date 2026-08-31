package main

import (
	"flag"
	"io"
	"runtime"
	"strings"

	"github.com/pmezard/go-difflib/difflib"

	goobersassets "github.com/goobers/goobers"
	"github.com/goobers/goobers/internal/agentkit"
	"github.com/goobers/goobers/internal/version"
)

const agentKitHelp = "Usage: goobers agent-kit <subcommand> [flags] [path]\n\n" +
	"Install, inspect, or explicitly update the release-matched Goobers agent\n" +
	"toolkit in a checked-in configuration repository.\n\n" +
	"Subcommands:\n" +
	"  install  install product-owned assets and a minimal harness reference\n" +
	"  check    report installed version, drift, missing files, and updates\n" +
	"  update   show a reviewable diff, then write only with --write\n\n" +
	"Default path is \".\". Targets must be repository roots and may not traverse\n" +
	"symbolic links or parent path segments.\n"

const agentKitInstallHelp = "Usage: goobers agent-kit install [--harness copilot|claude|generic] [path]\n\n" +
	"Install the toolkit bundled with this Goobers binary beneath the product-owned\n" +
	"`.goobers/agent-toolkit/` boundary and add a minimal adapter reference to the\n" +
	"selected harness instruction file. Existing instructions are preserved; install\n" +
	"only appends a clearly delimited managed reference when one is not already present.\n" +
	"Skills and other repository content are never overwritten.\n\n" +
	"Exit codes: 0 = installed or already current, 1 = unsafe target, collision, or\n" +
	"write error, 2 = usage error.\n"

const agentKitCheckHelp = "Usage: goobers agent-kit check [path]\n\n" +
	"Compare the installed manifest and product-owned file digests with the toolkit\n" +
	"bundled in this binary. Report the exact bundle and binary release identities,\n" +
	"modified or missing owned files, and whether an explicit update is available.\n\n" +
	"Exit codes: 0 = current, 1 = drift, missing manifest, available update, or\n" +
	"inspection error, 2 = usage error.\n"

const agentKitUpdateHelp = "Usage: goobers agent-kit update [--dry-run | --write [--replace-modified]] [path]\n\n" +
	"Show a reviewable diff from the repository's current files to the toolkit\n" +
	"bundled in this binary. The default and --dry-run never write. --write applies\n" +
	"only manifest-owned changes and preserves user-created files. If the installed\n" +
	"manifest has semantic drift or an owned file differs from its installed digest,\n" +
	"--replace-modified is also required.\n\n" +
	"Exit codes: 0 = diff shown or update written, 1 = unsafe target, ownership\n" +
	"collision, unacknowledged modification, or write error, 2 = usage error.\n"

func runAgentKit(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
		pf(stdout, "%s", agentKitHelp)
		return 0
	}
	if len(args) > 0 {
		pf(stderr, "error: unknown agent-kit command %q\n", args[0])
	}
	pf(stderr, "%s", agentKitHelp)
	return 2
}

func runAgentKitInstall(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("agent-kit install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	harness := fs.String("harness", "generic", "harness adapter: copilot, claude, or generic")
	fs.Usage = helpUsage(stderr, "agent-kit install")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	if !supportedAgentKitHarness(*harness) {
		pf(stderr, "error: unsupported harness %q (want copilot, claude, or generic)\n", *harness)
		return 2
	}

	bundle, err := currentAgentToolkitBundle()
	if err != nil {
		pf(stderr, "error: build bundled agent toolkit: %v\n", err)
		return 1
	}
	repository, err := openAgentToolkitRepository(fs.Args())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	result, err := repository.Install(bundle, *harness)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	if result.Installed {
		pf(stdout, "Installed agent toolkit bundle %s from Goobers %s (commit %s).\n",
			bundle.Manifest.BundleVersion,
			bundle.Manifest.Producer.Version,
			bundle.Manifest.Producer.Commit,
		)
	} else {
		pf(stdout, "Agent toolkit bundle %s is already current.\n", bundle.Manifest.BundleVersion)
	}
	switch {
	case result.InstructionCreated:
		pf(stdout, "Created %s with the %s adapter reference.\n", result.InstructionPath, *harness)
	case result.InstructionUpdated:
		pf(stdout, "Added the %s adapter reference to existing %s.\n", *harness, result.InstructionPath)
	default:
		pf(stdout, "%s already contains the %s adapter reference.\n", result.InstructionPath, *harness)
	}
	target := "."
	if fs.NArg() == 1 {
		target = fs.Arg(0)
	}
	writeAgentKitNextSteps(stdout, absolutePath(target), "")
	return 0
}

func runAgentKitCheck(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("agent-kit check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "agent-kit check")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}

	bundle, err := currentAgentToolkitBundle()
	if err != nil {
		pf(stderr, "error: build bundled agent toolkit: %v\n", err)
		return 1
	}
	repository, err := openAgentToolkitRepository(fs.Args())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	report, err := repository.Check(bundle)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	writeAgentKitCheckReport(stdout, report)
	if report.State != "current" {
		return 1
	}
	return 0
}

func runAgentKitUpdate(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("agent-kit update", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "show the update diff without writing")
	write := fs.Bool("write", false, "apply the displayed product-owned changes")
	replaceModified := fs.Bool("replace-modified", false, "acknowledge replacement of locally modified product-owned files")
	fs.Usage = helpUsage(stderr, "agent-kit update")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 || (*dryRun && *write) || (*replaceModified && !*write) {
		fs.Usage()
		return 2
	}

	bundle, err := currentAgentToolkitBundle()
	if err != nil {
		pf(stderr, "error: build bundled agent toolkit: %v\n", err)
		return 1
	}
	repository, err := openAgentToolkitRepository(fs.Args())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	plan, err := repository.PlanUpdate(bundle)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if err := writeAgentKitDiff(stdout, plan.Changes); err != nil {
		pf(stderr, "error: render agent toolkit diff: %v\n", err)
		return 1
	}
	if len(plan.UserCollisions) > 0 {
		pf(stderr, "error: update would overwrite user-owned files: %s\n", strings.Join(plan.UserCollisions, ", "))
		return 1
	}
	if !*write {
		if len(plan.ModifiedOwned) > 0 {
			pf(stdout, "\nWriting this update requires --write --replace-modified for: %s\n",
				strings.Join(plan.ModifiedOwned, ", "))
		}
		return 0
	}
	if err := repository.ApplyUpdate(plan, *replaceModified); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	pf(stdout, "\nUpdated agent toolkit to bundle %s from Goobers %s (commit %s).\n",
		bundle.Manifest.BundleVersion,
		bundle.Manifest.Producer.Version,
		bundle.Manifest.Producer.Commit,
	)
	return 0
}

func currentAgentToolkitBundle() (agentkit.Bundle, error) {
	info := version.Get()
	return agentkit.Build(goobersassets.AgentToolkitAssets, info.Version, info.Commit)
}

func openAgentToolkitRepository(args []string) (*agentkit.Repository, error) {
	target := "."
	if len(args) == 1 {
		target = args[0]
	}
	return agentkit.OpenRepository(target)
}

func supportedAgentKitHarness(harness string) bool {
	switch harness {
	case "copilot", "claude", "generic":
		return true
	default:
		return false
	}
}

func writeAgentKitCheckReport(w io.Writer, report agentkit.CheckReport) {
	pf(w, "state: %s\n", report.State)
	pf(w, "bundle version: %s\n", report.BundleVersion)
	pf(w, "source binary version: %s\n", report.SourceBinaryVersion)
	pf(w, "source binary commit: %s\n", report.SourceBinaryCommit)
	if report.InstalledBundleVersion == "" {
		pf(w, "installed bundle version: none\n")
		pf(w, "installed source version: none\n")
		pf(w, "installed source commit: none\n")
	} else {
		pf(w, "installed bundle version: %s\n", report.InstalledBundleVersion)
		pf(w, "installed source version: %s\n", report.InstalledSourceVersion)
		pf(w, "installed source commit: %s\n", report.InstalledSourceCommit)
	}
	pf(w, "update available: %s\n", yesNo(report.UpdateAvailable))
	writeAgentKitPathList(w, "modified owned files", report.Modified)
	writeAgentKitPathList(w, "missing owned files", report.Missing)
}

func writeAgentKitPathList(w io.Writer, label string, paths []string) {
	if len(paths) == 0 {
		pf(w, "%s: none\n", label)
		return
	}
	pf(w, "%s:\n", label)
	for _, path := range paths {
		pf(w, "  %s\n", path)
	}
}

func writeAgentKitDiff(w io.Writer, changes []agentkit.Change) error {
	if len(changes) == 0 {
		pf(w, "No agent toolkit changes.\n")
		return nil
	}
	for _, change := range changes {
		oldPath := "a/" + change.Path
		newPath := "b/" + change.Path
		if change.Kind == agentkit.ChangeAdd {
			oldPath = "/dev/null"
		}
		if change.Kind == agentkit.ChangeDelete {
			newPath = "/dev/null"
		}
		pf(w, "diff --goobers a/%s b/%s\n", change.Path, change.Path)
		switch change.Kind {
		case agentkit.ChangeAdd:
			pf(w, "new file mode %04o\n", change.Mode.Perm())
		case agentkit.ChangeDelete:
			pf(w, "deleted file mode %04o\n", change.OldMode.Perm())
		case agentkit.ChangeModify:
			if change.OldMode.Perm() != change.Mode.Perm() {
				pf(w, "old mode %04o\n", change.OldMode.Perm())
				pf(w, "new mode %04o\n", change.Mode.Perm())
			}
		}
		diff := difflib.UnifiedDiff{
			A:        difflib.SplitLines(string(change.Old)),
			B:        difflib.SplitLines(string(change.New)),
			FromFile: oldPath,
			ToFile:   newPath,
			Context:  3,
			Eol:      "\n",
		}
		if err := difflib.WriteUnifiedDiff(w, diff); err != nil {
			return err
		}
	}
	return nil
}

func writeAgentKitNextSteps(w io.Writer, target, instanceRoot string) {
	prompts := agentKitStarterPrompts(instanceRoot)
	pf(w, "\nStarter prompts:\n")
	// Wrapped in literal quotes via %s, not %q: %q also backslash-escapes the
	// string, and prompts[1] embeds a filesystem path. On POSIX that's a no-op
	// (no backslashes to escape), but on Windows it doubled every path
	// separator (C:\Users\... became C:\\Users\\...) — a starter prompt meant
	// to be copy-pasted verbatim then showed the wrong path.
	pf(w, "  Getting Started: \"%s\"\n", prompts[0])
	pf(w, "  Run Q&A: \"%s\"\n", prompts[1])
	pf(w, "  Upgrade: \"%s\"\n", prompts[2])
	pf(w, "\nToolkit maintenance:\n")
	commands := agentKitMaintenanceCommands(target, runtime.GOOS)
	pf(w, "  Check:  %s\n", commands[0])
	pf(w, "  Review: %s\n", commands[1])
	pf(w, "  Apply:  %s\n", commands[2])
}

func agentKitStarterPrompts(instanceRoot string) []string {
	instanceTarget := "<instance-path>"
	if strings.TrimSpace(instanceRoot) != "" {
		instanceTarget = absolutePath(instanceRoot)
	}
	return []string{
		"Use the Goobers Getting Started skill to inspect target repository <target-path-or-provider-url>, derive its default branch, CI command, toolchain, and conventions, and create the smallest validated configuration source here. Explain each write and ask only when required evidence or behavior cannot be safely derived.",
		"Use the Goobers run operator skill to summarize recent runs, issues, and pull requests for the Goobers instance at " + instanceTarget + ".",
		"Use the Goobers workflow upgrade skill to assess this config source for upgrade to the installed Goobers release.",
	}
}

func agentKitMaintenanceCommands(target, goos string) []string {
	quotedTarget := quoteShellArg(target, goos)
	return []string{
		"goobers agent-kit check " + quotedTarget,
		"goobers agent-kit update " + quotedTarget,
		"goobers agent-kit update --write " + quotedTarget,
	}
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}
