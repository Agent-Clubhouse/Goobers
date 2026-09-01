package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/version"
	"github.com/goobers/goobers/internal/worktree"

	"github.com/goobers/goobers/api/schemas"
)

const initHelp = "Usage: goobers init [--allow-ephemeral] [--guided [--instance-path <dir>] [--port=<port|auto>] [--no-open] [--workdir <dir>] | --demo [--insecure] | --template=quickstart [--harness <name>] [--source-tree <path> [--json]]] [path]\n\n" +
	"Scaffold an instance root at path (default \".\"): instance.yaml, config/\n" +
	"(seeded with a starter example), gaggles/, scheduler/, and a telemetry.db\n" +
	"placeholder. The daemon creates per-gaggle runs/ and workcopies/ under\n" +
	"gaggles/<gaggle>/ at runtime. Re-running is safe — existing pieces are left\n" +
	"untouched.\n" +
	"--guided opens the browser-based setup for a real repository and instance;\n" +
	"use --instance-path to select its instance root.\n" +
	"It prepares and validates configuration but does not run a workflow.\n" +
	"For GitHub PAT setup, use https://github.com/settings/personal-access-tokens/new,\n" +
	"select the repository's Resource owner, choose Only select repositories, and\n" +
	"grant the permissions documented in docs/guides/github-token-scopes.md.\n" +
	"--template=quickstart seeds the versioned onboarding workflow; it is\n" +
	"intentionally not production-safe. With --source-tree <path>, it instead\n" +
	"seeds the checked-in source layout (instance.yaml.example, manifest.yaml,\n" +
	"and gaggles/) without runtime state. The source-tree action is non-interactive,\n" +
	"preserves every existing file, and reports each created or skipped path;\n" +
	"--json emits its versioned machine-readable result envelope. With\n" +
	"--harness <name> (copilot or claude-code), every seeded goober uses that\n" +
	"harness, so the generated instance needs no goober.yaml edits to switch;\n" +
	"omitting it keeps the template's default harness. --demo seeds a hermetic mock-provider full-loop tour\n" +
	"requiring no repo, provider credentials, model tokens, or network writes. The\n" +
	"demo is supported on Linux and macOS, where network isolation is enforced; it is\n" +
	"fail-closed on Windows (no enforced network:none equivalent exists there) unless\n" +
	"--insecure is also given, which scaffolds the demo anyway and reports the\n" +
	"isolation limitation — an explicit, narrowly-scoped opt-in that does not alter\n" +
	"the general Windows sandbox policy (#651). Use `goobers preflight` to check and\n" +
	"launch the fully isolated WSL 2 route instead. --insecure requires --demo.\n" +
	"--allow-ephemeral permits initialization inside a linked or hosted workspace\n" +
	"only when that location is intentionally persistent; it is refused by default\n" +
	"to protect GitHub/App sessions whose worktrees may be deleted.\n"

func runInit(args []string, stdout, stderr io.Writer) int {
	return runInitWithInput(args, os.Stdin, stdout, stderr)
}

func runInitWithInput(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runInitWithInputForOS(args, stdin, stdout, stderr, runtime.GOOS)
}

func runInitWithInputForOS(args []string, stdin io.Reader, stdout, stderr io.Writer, goos string) int {
	fs := newCLIFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	demo := fs.Bool("demo", false, "seed a credential-free runnable demo workflow")
	insecure := fs.Bool("insecure", false, "with --demo on a platform without enforced network isolation (Windows), scaffold anyway without it")
	allowEphemeral := fs.Bool("allow-ephemeral", false, "allow initialization inside a linked or hosted ephemeral workspace")
	guided := fs.Bool("guided", false, "open browser-based setup for a real repository")
	guidedPort := fs.String("port", "auto", "with --guided, server port or auto")
	guidedNoOpen := fs.Bool("no-open", false, "with --guided, print the URL without opening a browser")
	guidedWorkdir := fs.String("workdir", defaultGettingStartedWorkdir(), "with --guided, temporary browser setup state")
	guidedInstancePath := fs.String("instance-path", "", "with --guided, instance root to create")
	template := fs.String("template", "", "seed a named onboarding template (available: quickstart)")
	harness := fs.String("harness", "", "with --template=quickstart, the agent harness every seeded goober uses (copilot, claude-code)")
	sourceTree := fs.String("source-tree", "", "seed the selected template as a checked-in config source at path")
	asJSON := fs.Bool("json", false, "emit the config-source action result as JSON")
	fs.Usage = helpUsage(stderr, "init")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	sourceTreeSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "source-tree" {
			sourceTreeSet = true
		}
	})
	selectedModes := 0
	for _, selected := range []bool{*guided, *demo, *template != ""} {
		if selected {
			selectedModes++
		}
	}
	if selectedModes > 1 {
		pf(stderr, "error: --guided, --demo, and --template cannot be combined\n")
		return 2
	}
	if *insecure && !*demo {
		pf(stderr, "error: --insecure requires --demo\n")
		return 2
	}
	if *template != "" && *template != instance.QuickstartTemplate {
		pf(stderr, "error: unknown init template %q (available: %s)\n", *template, instance.QuickstartTemplate)
		return 2
	}
	if sourceTreeSet && *sourceTree == "" {
		pf(stderr, "error: --source-tree destination must not be empty\n")
		return 2
	}
	if *sourceTree != "" && *template != instance.QuickstartTemplate {
		pf(stderr, "error: --source-tree requires --template=%s\n", instance.QuickstartTemplate)
		return 2
	}
	if *asJSON && *sourceTree == "" {
		pf(stderr, "error: --json is supported by init only with --source-tree\n")
		return 2
	}
	if *harness != "" && *template != instance.QuickstartTemplate {
		pf(stderr, "error: --harness requires --template=%s\n", instance.QuickstartTemplate)
		return 2
	}
	if *guidedInstancePath != "" && !*guided {
		pf(stderr, "error: --instance-path requires --guided\n")
		return 2
	}
	if err := instance.ValidateQuickstartHarness(*harness); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if *sourceTree != "" && fs.NArg() != 0 {
		pf(stderr, "error: --source-tree supplies the destination; do not also pass [path]\n")
		return 2
	}
	if *guided && fs.NArg() != 0 {
		pf(stderr, "error: --guided does not accept a path; use --instance-path <dir> to choose the instance location\n")
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	demoUnisolated := *demo && goos != "linux" && goos != "darwin"
	if demoUnisolated && !*insecure {
		pf(stderr, "error: --demo is supported only on Linux and macOS because enforced network isolation is unavailable on %s; run `goobers preflight` for the fully isolated WSL 2 route, or pass --insecure to proceed without isolation\n", goos)
		return 2
	}
	if *sourceTree != "" {
		return seedQuickstartConfigSource(*sourceTree, *harness, *asJSON, stdout, stderr, goos)
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}
	if *guided {
		browserArgs := []string{"--port=" + *guidedPort, "--workdir", *guidedWorkdir}
		if *guidedInstancePath != "" {
			browserArgs = append(browserArgs, "--instance-path", *guidedInstancePath)
		}
		if *guidedNoOpen {
			browserArgs = append(browserArgs, "--no-open")
		}
		if *allowEphemeral {
			browserArgs = append(browserArgs, "--allow-ephemeral")
		}
		return runGuidedInitBrowser(browserArgs, stdout, stderr)
	}
	if err := worktree.CheckInitTarget(context.Background(), root, *allowEphemeral); err != nil {
		pf(stderr, "error: %v\n", err)
		printInitTargetOverride(stderr, err)
		printDefaultedTargetNote(stderr, err, fs.NArg())
		return 2
	}

	var res *instance.InitResult
	var err error
	errCode := 2
	if *template == instance.QuickstartTemplate {
		res, err = instance.InitQuickstartWithOptions(root, instance.QuickstartOptions{Harness: *harness})
	} else if *demo {
		res, err = instance.InitDemo(root)
	} else {
		res, err = instance.Init(root)
	}
	if err != nil {
		pf(stderr, "error: %v\n", err)
		printDefaultedTargetNote(stderr, err, fs.NArg())
		return errCode
	}

	abs, err := filepath.Abs(res.Root)
	if err != nil {
		abs = res.Root
	}
	if len(res.Created) == 0 {
		pf(stdout, "instance already initialized at %s (nothing to do)\n", abs)
		if demoUnisolated {
			pln(stdout, demoInsecureWarning)
		}
		if code := finishInitValidation(root, stdout, stderr); code != 0 {
			return code
		}
		if err := ensureInitCompleted(root); err != nil {
			pf(stderr, "error: record successful init completion: %v\n", err)
			return 2
		}
		return 0
	}
	pf(stdout, "initialized instance at %s\n", abs)
	for _, c := range res.Created {
		pf(stdout, "  created  %s\n", c)
	}
	for _, s := range res.Skipped {
		pf(stdout, "  skipped  %s (already exists)\n", s)
	}
	pf(stdout, "\nLearn the desired-state model: %s\n", documentationURL("docs/concepts/README.md"))
	demoSeeded := false
	for _, created := range res.Created {
		if created == instance.ConfigDirName {
			demoSeeded = true
			break
		}
	}
	if *demo && demoSeeded {
		if demoUnisolated {
			pln(stdout, demoInsecureWarning)
		}
		pf(stdout, demoTourBanner, abs)
	}
	if code := finishInitValidation(root, stdout, stderr); code != 0 {
		return code
	}
	if err := ensureInitCompleted(root); err != nil {
		pf(stderr, "error: record successful init completion: %v\n", err)
		return 2
	}
	return 0
}

func finishInitValidation(root string, stdout, stderr io.Writer) int {
	pln(stdout, "\nPost-init validation:")
	if code := runValidate([]string{root}, stdout, stderr); code != 0 {
		pf(stderr, "error: initialized instance did not pass validation\n")
		return code
	}

	layout := instance.NewLayout(root)
	findings, err := findTemplatePlaceholders(root, layout.ConfigFile(), layout.ConfigDir())
	if err != nil {
		pf(stderr, "error: inspect initialized configuration placeholders: %v\n", err)
		return 2
	}
	if len(findings) == 0 {
		pln(stdout, "\nNext: no placeholder edits are required.")
		return 0
	}
	pln(stdout, "\nNext: edit these files before running a live workflow:")
	seen := make(map[string]bool, len(findings))
	for _, finding := range findings {
		if seen[finding.file] {
			continue
		}
		seen[finding.file] = true
		pf(stdout, "  %s\n", finding.file)
	}
	return 0
}

func printInitTargetOverride(stderr io.Writer, err error) {
	var unsafe *worktree.UnsafeInitTargetError
	if errors.As(err, &unsafe) {
		pf(stderr, "note: to acknowledge this target, rerun `goobers init --allow-ephemeral %q`\n", unsafe.Safety.Path)
	}
}

// printDefaultedTargetNote explains, after an init target-conflict refusal,
// that the conflicting target was never chosen explicitly — init with no
// [path] defaults to the current directory, the exact trap of running it from
// inside a source checkout (#2513).
func printDefaultedTargetNote(stderr io.Writer, err error, narg int) {
	var conflict *instance.TargetConflictError
	if narg == 0 && errors.As(err, &conflict) {
		pf(stderr, "note: no [path] argument was given, so the target defaulted to the current directory\n")
	}
}

func ensureInitCompleted(root string) error {
	instanceLog, _, err := journal.OpenInstanceLog(instance.NewLayout(root).SchedulerDir())
	if err != nil {
		return err
	}
	events, err := journal.ReadInstanceLog(instanceLog.Dir())
	if err != nil {
		_ = instanceLog.Close()
		return err
	}
	for _, event := range events {
		if event.Type == journal.EventInitCompleted {
			return instanceLog.Close()
		}
	}
	if err := instanceLog.Append(journal.Event{Type: journal.EventInitCompleted}); err != nil {
		_ = instanceLog.Close()
		return err
	}
	return instanceLog.Close()
}

func seedQuickstartConfigSource(root, harness string, asJSON bool, stdout, stderr io.Writer, goos string) int {
	envelope, err := executeSeedConfigSourceAction(root, harness, nil, goos)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	var validationOutput strings.Builder
	if code := runValidate(
		[]string{"--source-tree", "--json", envelope.Path},
		&validationOutput,
		&validationOutput,
	); code != 0 {
		pf(stderr, "error: seeded config source failed validation\n%s", validationOutput.String())
		return code
	}
	if asJSON {
		if err := encodeSchemaJSON(stdout, schemas.OnboardingAction, envelope); err != nil {
			pf(stderr, "error: encode onboarding action result: %v\n", err)
			return 1
		}
		return 0
	}

	pf(stdout, "seeded quickstart config source at %s\n", envelope.Path)
	for _, created := range envelope.Created {
		pf(stdout, "  created  %s\n", created)
	}
	for _, skipped := range envelope.Skipped {
		pf(stdout, "  skipped  %s (already exists)\n", skipped)
	}
	pf(stdout, "\nNext: %s\n", envelope.NextCommand)
	return 0
}

func quoteShellArg(arg, goos string) string {
	if goos == "windows" {
		// nextCommand targets PowerShell, where single-quoted strings are literal.
		return "'" + strings.ReplaceAll(arg, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(arg, "'", `'"'"'`) + "'"
}

func parseGitHubRepo(value string) (string, string, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "git@github.com:") {
		value = strings.TrimPrefix(value, "git@github.com:")
	} else if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		if !strings.EqualFold(parsed.Host, "github.com") {
			return "", "", fmt.Errorf("repository URL host must be github.com")
		}
		value = strings.TrimPrefix(parsed.Path, "/")
	}
	value = strings.TrimSuffix(value, ".git")
	parts := strings.Split(value, "/")
	if len(parts) != 2 || !githubRepoPart.MatchString(parts[0]) || !githubRepoPart.MatchString(parts[1]) {
		return "", "", fmt.Errorf("GitHub repository must be owner/name or a github.com URL")
	}
	return parts[0], parts[1], nil
}

var githubRepoPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func guidedGaggleName(repo string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(repo) {
		valid := r >= 'a' && r <= 'z' || r >= '0' && r <= '9'
		if valid {
			b.WriteRune(r)
			lastHyphen = false
		} else if !lastHyphen && b.Len() > 0 {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	name := strings.Trim(b.String(), "-")
	if name == "" {
		return "repository"
	}
	if len(name) > 50 {
		name = strings.TrimRight(name[:50], "-")
	}
	return name
}

// releaseVersionPattern matches a real tagged release — stable
// (vMAJOR.MINOR.PATCH) or pre-release (vMAJOR.MINOR.PATCH-beta.2 and
// similar SemVer 2.0.0 suffixes) — as opposed to a "dev" or bare-commit
// build, which has no tag to link docs against.
var releaseVersionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*)?$`)

func documentationURL(path string) string {
	ref := "main"
	if releaseVersionPattern.MatchString(version.Version) {
		ref = url.PathEscape(version.Version)
	}
	return fmt.Sprintf("https://github.com/Agent-Clubhouse/Goobers/blob/%s/%s", ref, path)
}

const demoTourBanner = `
Demo full loop (run these from %s):
  goobers run demo    # watch curate -> implement -> review -> merge preview
  goobers trace <id>  # inspect the journal and merge-preview artifact
`

// demoInsecureWarning is printed whenever --demo --insecure scaffolds a demo
// on a platform with no enforced network:none equivalent (issue #1545).
// Scaffolding is unconditional once --insecure opts in, but actually running
// the demo still requires the same trusted-local-execution env var every
// other network:none stage needs on Windows (internal/executor/
// network_windows.go) — this issue narrowly lifts the CLI-level refusal to
// even scaffold, it does not alter that general Windows sandbox policy.
const demoInsecureWarning = "\nwarning: demo scaffolded WITHOUT enforced network isolation — this platform\n" +
	"has no network:none equivalent. Before `goobers run demo`, set\n" +
	"GOOBERS_ALLOW_UNISOLATED_NETWORK_NONE=1 for trusted-local execution only.\n" +
	"For full isolation instead, run `goobers preflight` and launch the command through WSL 2.\n"
