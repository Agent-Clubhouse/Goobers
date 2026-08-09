package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/secretstore"
	"github.com/goobers/goobers/providers"
)

// `goobers connect` is the connect-your-repository rung of the onboarding
// ladder (docs/design/onboarding-first-value-ladder.md §3 R3, issue #2449):
// one non-interactive command that rewrites the template placeholders in
// instance.yaml and every materialized gaggle to point at a real repository,
// records the token env-var NAME (never a value), optionally seeds the
// labels the connected gaggles' backlog selectors actually require plus one
// starter issue, and then validates the result in-process.
const (
	connectAction          = "connect"
	connectDefaultTokenEnv = "GOOBERS_GITHUB_TOKEN"

	// connectPlaceholderOwner/Name are the template coordinates every shipped
	// template scaffolds (internal/instance/scaffold.go defaultConfig and the
	// starter/quickstart-v1 gaggle templates).
	connectPlaceholderOwner = "your-org"
	connectPlaceholderName  = "your-repo"

	// connectSeedVersion keeps the starter issue's dedupe marker stable so a
	// re-run of `goobers connect --seed` never files a duplicate.
	connectSeedVersion = "v1"
	connectSeedIssueID = "hello-goobers"
)

const connectSeedIssueTitle = "Hello Goobers: add a HELLO-GOOBERS.md introducing your new workforce"

const connectSeedIssueBody = "A safe first task for your new Goobers workforce: add a `HELLO-GOOBERS.md` " +
	"file at the repository root that introduces the workforce — what it is, which workflows are connected, " +
	"and where its configuration lives.\n\n" +
	"This issue only asks for a new documentation file, so it cannot break existing behavior; it exists to " +
	"prove the full issue -> implementation -> pull-request loop against your own repository.\n\n" +
	"Created by `goobers connect --seed`. Re-running the command is idempotent: the `goobers run-id` footer " +
	"below deduplicates, so no second copy of this issue is ever filed.\n"

const connectHelp = "Usage: goobers connect <owner>/<repo> [--token-env NAME] [--seed] [--replace] [--json] [path]\n\n" +
	"Connect an instance to your own GitHub repository — the connect rung of the\n" +
	"onboarding ladder. The command rewrites the template placeholders\n" +
	"(your-org/your-repo) in instance.yaml repos[] and in every materialized\n" +
	"gaggle's project and backlog under config/gaggles/, then validates the\n" +
	"result in-process. Configuration already pointing at a real repository is\n" +
	"left alone (and reported skipped) unless --replace is set.\n\n" +
	"Credentials are recorded by NAME only: --token-env stores the environment\n" +
	"variable name (default " + connectDefaultTokenEnv + ") in the repo's token\n" +
	"reference. Token values never pass through this command; a value that looks\n" +
	"like a pasted token is rejected.\n\n" +
	"--seed derives the labels the connected gaggles' backlog selectors actually\n" +
	"require (backlog labels plus each workflow's trustLabel/requireLabels\n" +
	"inputs), idempotently ensures they exist on the repository, and files one\n" +
	"safe starter issue carrying exactly those labels. Seeding uses the same\n" +
	"--token-env; when that variable is unset the issue is reported pending and\n" +
	"the local rewrite still completes.\n\n" +
	"When the token variable is set, the target repository's reachability is\n" +
	"also checked with the exact credential path a real run would use.\n\n" +
	"Flags:\n" +
	"  --token-env <name>  repository token environment variable name (default " + connectDefaultTokenEnv + ")\n" +
	"  --seed              ensure selector labels + one starter issue on the repository\n" +
	"  --replace           also rewrite entries already pointing at a real repository\n" +
	"  --json              emit the versioned onboarding action envelope\n\n" +
	"Exit codes: 0 = connected (or already connected), 1 = refusal, validation,\n" +
	"or provider error, 2 = usage error.\n"

// connectFlagArgs lets connect's flags appear after the positional repo/path,
// as documented. The standard flag package otherwise stops parsing at the
// first positional argument.
func connectFlagArgs(args []string) []string {
	flagNames := map[string]bool{"seed": true, "replace": true, "json": true}
	valueFlags := map[string]bool{"token-env": true}
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, len(args))
	expectValue := false
	for _, arg := range args {
		if expectValue {
			flags = append(flags, arg)
			expectValue = false
			continue
		}
		trimmed := strings.TrimLeft(arg, "-")
		dashes := len(arg) - len(trimmed)
		name, _, hasValue := strings.Cut(trimmed, "=")
		if dashes > 0 && (flagNames[name] || valueFlags[name]) {
			flags = append(flags, arg)
			expectValue = valueFlags[name] && !hasValue
			continue
		}
		positionals = append(positionals, arg)
	}
	return append(flags, positionals...)
}

func runConnect(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("connect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	tokenEnv := fs.String("token-env", connectDefaultTokenEnv, "repository token environment variable name")
	seed := fs.Bool("seed", false, "ensure selector labels and one starter issue on the repository")
	replace := fs.Bool("replace", false, "also rewrite entries already pointing at a real repository")
	jsonOutput := fs.Bool("json", false, "emit the versioned onboarding action envelope")
	fs.Usage = helpUsage(stderr, "connect")
	if err := fs.Parse(connectFlagArgs(args)); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	owner, name, err := parseGitHubRepo(fs.Arg(0))
	if err != nil {
		pf(stderr, "error: %v (GitHub is the only supported provider in v1)\n", err)
		return 2
	}
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}
	// The guided paste-guard, verbatim: a --token-env value that looks like a
	// GitHub token (ghp_/github_pat_/... prefixes) is a pasted secret, not an
	// environment variable name. Values never pass through connect.
	if !instance.ValidGuidedTokenEnvName(*tokenEnv) {
		pf(stderr, "error: repository token environment variable must name a valid environment variable; do not provide a token value\n")
		return 2
	}
	return executeConnect(connectOptions{
		owner:    owner,
		name:     name,
		root:     root,
		tokenEnv: *tokenEnv,
		seed:     *seed,
		replace:  *replace,
		json:     *jsonOutput,
	}, stdout, stderr)
}

type connectOptions struct {
	owner    string
	name     string
	root     string
	tokenEnv string
	seed     bool
	replace  bool
	json     bool
}

func executeConnect(opts connectOptions, stdout, stderr io.Writer) int {
	layout := instance.NewLayout(opts.root)
	configFile := layout.ConfigFile()
	if _, err := os.Stat(configFile); err != nil {
		pf(stderr, "error: %s not found (not an instance root — run `goobers init` first)\n", configFile)
		return 2
	}
	cfg, err := instance.LoadConfig(configFile)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if cfg.WorkflowSource != nil {
		pf(stderr, "error: this instance materializes config from a source tree; edit the source and run goobers config materialize\n")
		return 1
	}

	result := onboardingActionResult{
		Action:  connectAction,
		Version: onboardingActionVersion,
		Created: []string{},
		Updated: []string{},
		Skipped: []string{},
		Path:    absolutePath(opts.root),
	}

	instanceChanged, err := connectRewriteInstanceConfig(cfg, opts)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if instanceChanged {
		if err := instance.WriteConfig(configFile, cfg); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		result.Updated = append(result.Updated, instance.ConfigFileName)
	} else {
		result.Skipped = append(result.Skipped, instance.ConfigFileName)
	}

	gaggleFiles, err := filepath.Glob(filepath.Join(layout.ConfigDir(), "gaggles", "*", "gaggle.yaml"))
	if err != nil {
		pf(stderr, "error: list materialized gaggles: %v\n", err)
		return 1
	}
	sort.Strings(gaggleFiles)
	for _, path := range gaggleFiles {
		display := diagnosticFile(opts.root, path)
		changed, err := connectRewriteGaggleFile(path, opts.owner, opts.name, opts.replace)
		if err != nil {
			pf(stderr, "error: %s: %v\n", display, err)
			return 1
		}
		if changed {
			result.Updated = append(result.Updated, display)
		} else {
			result.Skipped = append(result.Skipped, display)
		}
	}

	// Post-write validation, in-process — exactly what `goobers validate
	// <path>` would report. Fail closed: connect must never leave an instance
	// it cannot vouch for.
	var validation strings.Builder
	if code := runValidate([]string{opts.root}, &validation, &validation); code != 0 {
		pf(stderr, "%s", validation.String())
		pf(stderr, "error: connected instance failed validation; fix the findings above and re-run `goobers validate %s`\n",
			quoteShellArg(result.Path, runtime.GOOS))
		return code
	}

	tokenSet := os.Getenv(opts.tokenEnv) != ""
	checksPassed := false
	if tokenSet {
		reloaded, err := instance.LoadConfig(configFile)
		if err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
		stores, err := secretstore.NewRegistry(reloaded.SecretStores)
		if err != nil {
			pf(stderr, "error: secretStores: %v\n", err)
			return 1
		}
		var scoped []instance.RepoRef
		for _, repo := range reloaded.Repos {
			if repo.Provider == "github" && repo.Owner == opts.owner && repo.Name == opts.name {
				scoped = append(scoped, repo)
			}
		}
		var checkOutput strings.Builder
		if !checkTargetRepositoriesAtFile(scoped, stores, &checkOutput, diagnosticFile(opts.root, configFile)) {
			pf(stderr, "%s", checkOutput.String())
			pf(stderr, "error: %s/%s is not reachable with the credential named by %s; fix the token or repository access and re-run `goobers validate --check-repos %s`\n",
				opts.owner, opts.name, opts.tokenEnv, quoteShellArg(result.Path, runtime.GOOS))
			return 1
		}
		if !opts.json {
			pf(stdout, "%s", checkOutput.String())
		}
		checksPassed = true
	}

	connectedWorkflow := ""
	if opts.seed || checksPassed {
		set, report, err := instance.LoadConfigDir(layout.ConfigDir())
		if err != nil {
			pf(stderr, "error: load connected configuration: %v (report: %+v)\n", err, report)
			return 1
		}
		labels, workflow := connectSelectorLabels(set, opts.owner, opts.name)
		connectedWorkflow = workflow
		if opts.seed {
			if code := connectSeedRepository(opts, labels, &result, stderr); code != 0 {
				return code
			}
		}
	}

	result.NextCommand = connectNextCommand(result.Path, connectedWorkflow, checksPassed)

	if opts.json {
		if err := encodeSchemaJSON(stdout, schemas.OnboardingAction, result); err != nil {
			pf(stderr, "error: encode connect action result: %v\n", err)
			return 1
		}
		return 0
	}
	printConnectResult(stdout, opts, result)
	return 0
}

// connectNextCommand picks the honest next rung: a real run when validation
// and the scoped repository check both passed, the validate command with the
// deeper checks otherwise.
func connectNextCommand(path, workflow string, checksPassed bool) string {
	quoted := quoteShellArg(path, runtime.GOOS)
	if checksPassed && workflow != "" {
		return "goobers run " + workflow + " " + quoted
	}
	return "goobers validate --check-harness --check-repos " + quoted
}

// connectRewriteInstanceConfig points cfg's repos[] at the requested
// repository via the standard LoadConfig/WriteConfig round-trip. It reports
// whether cfg changed, and fails closed when nothing carries a placeholder
// and --replace was not given.
func connectRewriteInstanceConfig(cfg *instance.Config, opts connectOptions) (bool, error) {
	target := instance.RepoRef{
		Provider: "github",
		Owner:    opts.owner,
		Name:     opts.name,
		Token:    instance.TokenRef{Env: opts.tokenEnv},
	}
	for i := range cfg.Repos {
		repo := cfg.Repos[i]
		if repo.Owner == connectPlaceholderOwner && repo.Name == connectPlaceholderName {
			cfg.Repos[i] = target
			return true, nil
		}
		if repo.Provider == "github" && repo.Owner == opts.owner && repo.Name == opts.name {
			if repo.Token.Env == opts.tokenEnv {
				return false, nil // already connected
			}
			if opts.replace {
				cfg.Repos[i] = target
				return true, nil
			}
			return false, fmt.Errorf(
				"repos[%d] already names %s/%s but reads its token from %s, not %s; re-run with --replace to rewrite it",
				i, repo.Owner, repo.Name, describeTokenRef(repo.Token), opts.tokenEnv,
			)
		}
	}
	if !opts.replace {
		return false, fmt.Errorf(
			"no placeholder repository entry found in %s; currently configured: %s — re-run with --replace to rewrite repos[0]",
			instance.ConfigFileName, describeConfiguredRepos(cfg.Repos),
		)
	}
	if len(cfg.Repos) == 0 {
		cfg.Repos = []instance.RepoRef{target}
		return true, nil
	}
	cfg.Repos[0] = target
	return true, nil
}

func describeTokenRef(token instance.TokenRef) string {
	switch {
	case token.Env != "":
		return "env " + token.Env
	case token.File != "":
		return "file " + token.File
	case token.Keychain != "":
		return "keychain " + token.Keychain
	case token.Store != "":
		return "store " + token.Store
	}
	return "no token source"
}

func describeConfiguredRepos(repos []instance.RepoRef) string {
	if len(repos) == 0 {
		return "no repositories"
	}
	entries := make([]string, 0, len(repos))
	for i, repo := range repos {
		entries = append(entries, fmt.Sprintf("repos[%d] %s/%s", i, repo.Owner, repo.Name))
	}
	return strings.Join(entries, ", ")
}

// connectRewriteGaggleFile replaces the placeholder project/backlog
// coordinates in one materialized gaggle.yaml with owner/name, using
// comment-preserving yaml.v3 node surgery. A gaggle already pointing at a
// real repository is left untouched (reported skipped by the caller) unless
// replace is set. It reports whether the file was rewritten.
func connectRewriteGaggleFile(path, owner, name string, replace bool) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, fmt.Errorf("parse: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return false, fmt.Errorf("parse: not a YAML document")
	}
	rootNode := doc.Content[0]
	spec := yamlMapValue(rootNode, "spec")
	if spec == nil {
		return false, fmt.Errorf("no spec mapping")
	}

	changed := false
	if project := yamlMapValue(spec, "project"); project != nil {
		ownerNode := yamlMapValue(project, "owner")
		nameNode := yamlMapValue(project, "name")
		if ownerNode != nil && nameNode != nil {
			placeholder := ownerNode.Value == connectPlaceholderOwner && nameNode.Value == connectPlaceholderName
			current := ownerNode.Value == owner && nameNode.Value == name
			if (placeholder || replace) && !current {
				ownerNode.Value = owner
				nameNode.Value = name
				changed = true
			}
		}
	}
	if backlog := yamlMapValue(spec, "backlog"); backlog != nil {
		if projectNode := yamlMapValue(backlog, "project"); projectNode != nil {
			placeholder := projectNode.Value == connectPlaceholderOwner+"/"+connectPlaceholderName
			targetValue := owner + "/" + name
			if (placeholder || replace) && projectNode.Value != targetValue {
				projectNode.Value = targetValue
				changed = true
			}
		}
	}
	if !changed {
		return false, nil
	}

	var out strings.Builder
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(&doc); err != nil {
		return false, fmt.Errorf("encode: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return false, fmt.Errorf("encode: %w", err)
	}
	if err := os.WriteFile(path, []byte(out.String()), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// yamlMapValue returns the value node for key in a yaml.v3 mapping node.
func yamlMapValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// connectSelectorLabels derives the exact label set the connected gaggles'
// backlog selectors need for an item to be eligible: the gaggle's
// spec.backlog.labels, plus every workflow's trustLabel/requireLabels inputs
// (falling back to the gaggle's spec.requireLabels default when a workflow
// declares no requireLabels of its own). It also returns the first connected
// workflow's name for the next-command hint.
func connectSelectorLabels(set *instance.ConfigSet, owner, name string) ([]string, string) {
	seen := map[string]bool{}
	var labels []string
	add := func(label string) {
		label = strings.TrimSpace(label)
		if label != "" && !seen[label] {
			seen[label] = true
			labels = append(labels, label)
		}
	}
	workflow := ""
	for _, gaggle := range set.Gaggles {
		project := gaggle.Spec.Project
		if project.Owner != owner || project.Name != name {
			continue
		}
		for _, label := range gaggle.Spec.Backlog.Labels {
			add(label)
		}
		for _, flow := range set.Workflows {
			if flow.Spec.Gaggle != gaggle.Name {
				continue
			}
			if workflow == "" {
				workflow = flow.Name
			}
			flowDeclaresRequire := false
			for _, task := range flow.Spec.Tasks {
				if trust := task.Inputs["trustLabel"]; trust != "" {
					add(trust)
				}
				if require, ok := task.Inputs["requireLabels"]; ok {
					flowDeclaresRequire = true
					for _, label := range splitLabelList(require) {
						add(label)
					}
				}
			}
			if !flowDeclaresRequire {
				for _, label := range gaggle.Spec.RequireLabels {
					add(label)
				}
			}
		}
	}
	sort.Strings(labels)
	return labels, workflow
}

// connectSeedCatalog shapes the selector-derived labels and the single
// starter issue as an onboarding seed catalog, so seeding reuses the exact
// idempotent machinery stub-sample ships (EnsureWorkItemLabels, the run-id
// dedupe marker scan, CreateWorkItem).
func connectSeedCatalog(labels []string) onboardingSeedCatalog {
	catalogLabels := make([]providers.WorkItemLabel, 0, len(labels))
	for _, label := range labels {
		catalogLabels = append(catalogLabels, providers.WorkItemLabel{
			Name:        label,
			Color:       "0E8A16",
			Description: "Eligible for Goobers agentic work (seeded by goobers connect)",
		})
	}
	return onboardingSeedCatalog{
		SchemaVersion: 1,
		Sample:        onboardingSeedSample{ID: connectAction, Version: connectSeedVersion},
		Labels:        catalogLabels,
		Issues: []onboardingSeedIssue{{
			ID:     connectSeedIssueID,
			Title:  connectSeedIssueTitle,
			Body:   connectSeedIssueBody,
			Labels: labels,
		}},
	}
}

// connectSeedRepository ensures the selector-derived labels exist and files
// the starter issue, degrading exactly like stub-sample when the token env
// is unset: the issue is reported pending and the exit stays 0. It uses the
// SAME token env the connect recorded — deliberately closing the historical
// GOOBERS_GITHUB_ISSUES_TOKEN vs GOOBERS_GITHUB_TOKEN fork.
func connectSeedRepository(opts connectOptions, labels []string, result *onboardingActionResult, stderr io.Writer) int {
	catalog := connectSeedCatalog(labels)
	if os.Getenv(opts.tokenEnv) == "" {
		appendPendingSeedIssues(result, catalog, "credentials unavailable")
		return 0
	}
	repository := providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    opts.owner,
		Name:     opts.name,
	}
	ctx, cancel := context.WithTimeout(context.Background(), stubSampleProviderTimeout)
	defer cancel()
	seeder := newOnboardingIssueSeeder(os.Getenv(opts.tokenEnv))
	if err := seedOnboardingIssuesAs(ctx, seeder, repository, catalog, connectAction, result); err != nil {
		pf(stderr, "error: seed %s/%s: %v\n", opts.owner, opts.name, err)
		return 1
	}
	return 0
}

func printConnectResult(stdout io.Writer, opts connectOptions, result onboardingActionResult) {
	pf(stdout, "connected %s/%s at %s\n", opts.owner, opts.name, result.Path)
	for _, item := range result.Updated {
		pf(stdout, "  updated  %s\n", item)
	}
	for _, item := range result.Created {
		pf(stdout, "  created  %s\n", item)
	}
	for _, item := range result.Skipped {
		if strings.Contains(item, "(pending:") {
			pf(stdout, "  pending  %s\n", item)
		} else {
			pf(stdout, "  skipped  %s\n", item)
		}
	}
	pf(stdout, "next: %s\n", result.NextCommand)
}

// connectedRepository reports the repository an instance root is connected
// to, or "" when the instance does not exist, fails to load, or still
// carries the template placeholder. Shared with the guided server's state
// endpoint.
func connectedRepository(root string) string {
	configFile := instance.NewLayout(root).ConfigFile()
	if _, err := os.Stat(configFile); err != nil {
		return ""
	}
	cfg, err := instance.LoadConfig(configFile)
	if err != nil {
		return ""
	}
	for _, repo := range cfg.Repos {
		if repo.Owner == connectPlaceholderOwner && repo.Name == connectPlaceholderName {
			continue
		}
		if repo.Provider == "github" && repo.Owner != "" && repo.Name != "" {
			return repo.Owner + "/" + repo.Name
		}
	}
	return ""
}

// connectedTokenEnv reports the environment variable name `goobers connect`
// recorded for the connected repository's credential, or "" when the
// instance has no connected repository yet. Shared with the guided server's
// state endpoint and run-dispatch preflight (#2639) — this is the one
// authoritative source for "which env var name actually matters," so both
// read paths agree with what `connect --token-env` actually persisted
// instead of assuming the default name.
func connectedTokenEnv(root string) string {
	configFile := instance.NewLayout(root).ConfigFile()
	if _, err := os.Stat(configFile); err != nil {
		return ""
	}
	cfg, err := instance.LoadConfig(configFile)
	if err != nil {
		return ""
	}
	for _, repo := range cfg.Repos {
		if repo.Owner == connectPlaceholderOwner && repo.Name == connectPlaceholderName {
			continue
		}
		if repo.Provider == "github" && repo.Owner != "" && repo.Name != "" {
			return repo.Token.Env
		}
	}
	return ""
}
