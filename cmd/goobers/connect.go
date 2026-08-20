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
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
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

// Stable diagnostic codes for connect's own refusals and notes. They are
// greppable identities for the three ways a connect can be wrong before it is
// ever run, in the same spirit as validate's REPO001/PLACEHOLDER001 codes.
const (
	// connectADOIdentityCode: the positional argument names an Azure DevOps
	// repository, which connect cannot provision (cold-start ado #7).
	connectADOIdentityCode = "CONNECT001"
	// connectForeignProviderCode: the repos[] entry connect would rewrite
	// declares a provider other than github, so the rewrite would silently
	// re-provider an existing instance.
	connectForeignProviderCode = "CONNECT002"
	// connectUnreachableCode: the pre-write GitHub reachability preflight
	// failed, so nothing was written.
	connectUnreachableCode = "CONNECT003"
	// connectSelectorRealityCode: the connect succeeded but the connected
	// gaggles' selectors match none of the repository's open issues.
	connectSelectorRealityCode = "CONNECT004"
)

// connectADOTokenEnvHint is the token variable the Azure DevOps refusal
// suggests when the caller kept connect's GitHub default (or handed us
// something that is not a usable variable name — a pasted value must never be
// echoed back into a diagnostic).
const connectADOTokenEnvHint = "GOOBERS_ADO_TOKEN"

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
	"Only GitHub repositories can be connected. An Azure DevOps identity (an\n" +
	"organization/project/repository slug or a dev.azure.com URL) is refused\n" +
	"with the instance.yaml block to write by hand — see\n" +
	"docs/guides/ado-authentication.md.\n\n" +
	"--seed derives two label sets from the connected gaggles and idempotently\n" +
	"ensures every one of them exists on the repository: the backlog SELECTORS\n" +
	"(backlog labels plus each workflow's trustLabel/requireLabels inputs) and\n" +
	"the labels those workflows WRITE or exclude on (the goobers:claimed claim\n" +
	"mirror, each issue-close-out stage's park/status label, readyLabel inputs,\n" +
	"and every excludeLabels entry) — a run dies at its first park or close-out\n" +
	"when those do not exist yet. The one safe starter issue it files carries\n" +
	"the selector labels only, never the lifecycle ones. Seeding uses the same\n" +
	"--token-env; when that variable is unset the issue is reported pending and\n" +
	"the local rewrite still completes.\n\n" +
	"When the token variable is set, the target repository's reachability is\n" +
	"checked with the exact credential path a real run would use BEFORE any\n" +
	"file is written, and a failed connect leaves the instance exactly as it\n" +
	"was. After a successful connect the same credential reports how many of\n" +
	"the repository's open issues your backlog selectors currently match.\n\n" +
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

// connectADORepo is an Azure DevOps repository identity: the three-part
// organization/project/repository coordinate instance.yaml's repos[] entry and
// a gaggle's spec.project both carry (internal/instance/config.go RepoRef,
// docs/guides/ado-authentication.md).
type connectADORepo struct {
	Organization string
	Project      string
	Repository   string
}

func (r connectADORepo) String() string {
	return r.Organization + "/" + r.Project + "/" + r.Repository
}

// Azure DevOps organization names are alphanumeric with hyphens — never
// dotted — which is what keeps a host-shaped first segment ("github.com/acme/
// web") out of the three-part branch below. Project and repository names are
// looser (dots and underscores are legal).
var (
	adoOrganizationPart = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	adoNamePart         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// connectADOIdentity recognizes an Azure DevOps repository in the forms an
// operator is likely to hand `goobers connect`: the bare three-part
// organization/project/repository slug that `goobers validate` already renders
// in its own diagnostics, the dev.azure.com web URL, the legacy
// <organization>.visualstudio.com URL, and the ssh.dev.azure.com remote. It
// reports false for anything it cannot resolve to all three coordinates, so
// the caller's generic GitHub refusal still covers gitlab.com, typos, and
// bare owners.
func connectADOIdentity(value string) (connectADORepo, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return connectADORepo{}, false
	}
	if rest, found := strings.CutPrefix(value, "git@ssh.dev.azure.com:v3/"); found {
		return adoIdentityFromSegments(strings.Split(strings.TrimSuffix(rest, ".git"), "/"))
	}
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		host := strings.ToLower(parsed.Hostname())
		segments := adoPathSegments(parsed.Path)
		switch {
		case host == "dev.azure.com" || host == "ssh.dev.azure.com":
			// https://dev.azure.com/<org>/<project>/_git/<repo>, and the
			// short form ADO itself emits when project and repository share
			// a name: https://dev.azure.com/<org>/_git/<repo>.
			if len(segments) == 2 {
				segments = []string{segments[0], segments[1], segments[1]}
			}
			return adoIdentityFromSegments(segments)
		case strings.HasSuffix(host, ".visualstudio.com"):
			organization := strings.TrimSuffix(host, ".visualstudio.com")
			if len(segments) == 1 {
				segments = []string{segments[0], segments[0]}
			}
			return adoIdentityFromSegments(append([]string{organization}, segments...))
		}
		return connectADORepo{}, false
	}
	return adoIdentityFromSegments(strings.Split(strings.TrimSuffix(value, ".git"), "/"))
}

// adoPathSegments splits an Azure DevOps URL path into identity segments,
// dropping the "_git" marker and the legacy DefaultCollection.
func adoPathSegments(path string) []string {
	var segments []string
	for _, segment := range strings.Split(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/") {
		decoded, err := url.PathUnescape(segment)
		if err == nil {
			segment = decoded
		}
		if segment == "" || segment == "_git" || strings.EqualFold(segment, "DefaultCollection") {
			continue
		}
		segments = append(segments, segment)
	}
	return segments
}

func adoIdentityFromSegments(segments []string) (connectADORepo, bool) {
	trimmed := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment = strings.TrimSpace(segment); segment != "" {
			trimmed = append(trimmed, segment)
		}
	}
	if len(trimmed) != 3 ||
		!adoOrganizationPart.MatchString(trimmed[0]) ||
		!adoNamePart.MatchString(trimmed[1]) ||
		!adoNamePart.MatchString(trimmed[2]) {
		return connectADORepo{}, false
	}
	return connectADORepo{Organization: trimmed[0], Project: trimmed[1], Repository: trimmed[2]}, true
}

// connectADOTokenEnv picks the token variable name the refusal prints. A
// caller that kept connect's GitHub default gets the ADO-shaped suggestion,
// and a value that is not a legal variable name (a pasted secret) is never
// echoed back.
func connectADOTokenEnv(tokenEnv string) string {
	tokenEnv = strings.TrimSpace(tokenEnv)
	if tokenEnv == "" || tokenEnv == connectDefaultTokenEnv || !instance.ValidGuidedTokenEnvName(tokenEnv) {
		return connectADOTokenEnvHint
	}
	return tokenEnv
}

// connectADORefusal is the CONNECT001 diagnostic: what is wrong, what it would
// cost at runtime, and the exact file edits that replace the connect rung for
// an Azure DevOps instance. The engine itself is provider-neutral — only this
// onboarding command is GitHub-shaped — so the remedy is a hand-written
// config, not a missing feature.
func connectADORefusal(repo connectADORepo, tokenEnv string) string {
	return fmt.Sprintf("error: %s %q is an Azure DevOps organization/project/repository identity; "+
		"`goobers connect` writes provider: github entries only, so connecting it would record an Azure DevOps "+
		"organization as a github.com repository and every run would fail at repository reachability.\n"+
		"Azure DevOps instances are configured by hand (the engine itself is provider-neutral):\n"+
		"  1. %s — replace the repos[] entry with:\n"+
		"       repos:\n"+
		"         - provider: ado\n"+
		"           owner: %s\n"+
		"           project: %s\n"+
		"           name: %s\n"+
		"           auth:\n"+
		"             kind: %s            # or %s, %s, %s\n"+
		"           token:\n"+
		"             env: %s\n"+
		"  2. each config/gaggles/*/gaggle.yaml — set spec.project to the same four fields and\n"+
		"     spec.backlog.project to the Azure Boards project.\n"+
		"  3. run `goobers validate --strict --check-repos <path>`; it checks the ADO credential and\n"+
		"     reachability directly. `--seed` is GitHub-only, so seed Azure Boards tags by hand.\n"+
		"See docs/guides/ado-authentication.md for the credential kinds and\n"+
		"reference-workflows/instance.yaml.example for the surrounding file.\n",
		connectADOIdentityCode, repo.String(), instance.ConfigFileName,
		repo.Organization, repo.Project, repo.Repository,
		instance.ADOAuthPAT, instance.ADOAuthAzureCLI, instance.ADOAuthWorkloadIdentity, instance.ADOAuthManagedIdentity,
		connectADOTokenEnv(tokenEnv),
	)
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
		// The two-part guess at an Azure DevOps repository is accepted by
		// parseGitHubRepo and used to be written to disk as a provider:
		// github entry pointing at an ADO organization (cold-start ado #7);
		// the honest three-part identity got a bare "v1" refusal that named
		// no way forward. Recognize the ADO forms and hand back the exact
		// manual steps instead. The two-part guess is caught later, before
		// any write, by the reachability preflight.
		if ado, ok := connectADOIdentity(fs.Arg(0)); ok {
			pf(stderr, "%s", connectADORefusal(ado, *tokenEnv))
			return 2
		}
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

	// Plan the instance.yaml rewrite in memory only. Nothing reaches disk
	// until the provider guard here and the reachability preflight below have
	// both passed: connect used to write first and check afterwards, which is
	// how a two-part guess at an Azure DevOps repository left a permanently
	// broken provider: github config behind (cold-start ado #7).
	instanceChanged, err := connectRewriteInstanceConfig(cfg, opts)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}

	tokenSet := os.Getenv(opts.tokenEnv) != ""
	checksPassed := false
	if tokenSet {
		stores, err := secretstore.NewRegistry(cfg.SecretStores)
		if err != nil {
			pf(stderr, "error: secretStores: %v\n", err)
			return 1
		}
		var scoped []instance.RepoRef
		for _, repo := range cfg.Repos {
			if repo.Provider == "github" && repo.Owner == opts.owner && repo.Name == opts.name {
				scoped = append(scoped, repo)
			}
		}
		var checkOutput strings.Builder
		if !checkTargetRepositoriesAtFile(scoped, stores, &checkOutput, diagnosticFile(opts.root, configFile)) {
			pf(stderr, "%s", checkOutput.String())
			pf(stderr, "error: %s %s/%s is not reachable with the credential named by %s; nothing was written. "+
				"Fix the token or repository access, or — if this is not a GitHub repository — configure %s by hand "+
				"(docs/guides/ado-authentication.md for Azure DevOps), then re-run `goobers connect`\n",
				connectUnreachableCode, opts.owner, opts.name, opts.tokenEnv, instance.ConfigFileName)
			return 1
		}
		if !opts.json {
			pf(stdout, "%s", checkOutput.String())
		}
		checksPassed = true
	}

	// Everything below writes. A restore point holds the previous bytes of
	// every file connect touches so a failure after the first write puts the
	// instance back exactly as it was rather than leaving a half-connected
	// tree (verify-then-write for the preflight above, atomic restore here).
	restore := &connectRestorePoint{}
	if instanceChanged {
		if err := restore.snapshot(configFile); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
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
		pf(stderr, "error: list materialized gaggles: %v%s\n", err, restore.rollback())
		return 1
	}
	sort.Strings(gaggleFiles)
	for _, path := range gaggleFiles {
		display := diagnosticFile(opts.root, path)
		if err := restore.snapshot(path); err != nil {
			pf(stderr, "error: %s: %v%s\n", display, err, restore.rollback())
			return 1
		}
		changed, err := connectRewriteGaggleFile(path, opts.owner, opts.name, opts.replace)
		if err != nil {
			pf(stderr, "error: %s: %v%s\n", display, err, restore.rollback())
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
	// it cannot vouch for, so a failing instance is restored rather than left
	// half-connected for the next run to trip over.
	var validation strings.Builder
	if code := runValidate([]string{opts.root}, &validation, &validation); code != 0 {
		rolledBack := restore.rollback()
		pf(stderr, "%s", validation.String())
		pf(stderr, "error: connected instance failed validation%s; the findings above describe the attempted "+
			"connection — fix them and re-run `goobers connect %s/%s %s`\n",
			rolledBack, opts.owner, opts.name, quoteShellArg(result.Path, runtime.GOOS))
		return code
	}

	connectedWorkflow := ""
	var selectors []string
	if opts.seed || checksPassed {
		set, report, err := instance.LoadConfigDir(layout.ConfigDir())
		if err != nil {
			pf(stderr, "error: load connected configuration: %v (report: %+v)\n", err, report)
			return 1
		}
		derivedSelectors, applied, workflow := connectDerivedLabels(set, opts.owner, opts.name)
		selectors, connectedWorkflow = derivedSelectors, workflow
		if opts.seed {
			if code := connectSeedRepository(opts, selectors, applied, &result, stderr); code != 0 {
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
		if checksPassed {
			connectReportSelectorReality(opts, selectors, stdout, stderr)
		}
		return 0
	}
	printConnectResult(stdout, opts, result)
	// The reality echo lands after the result so a successful connect reads as
	// "connected ... / next: ...", then the caveat.
	if checksPassed {
		connectReportSelectorReality(opts, selectors, stdout, stderr)
	}
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

// connectRestorePoint remembers the previous bytes of every file connect
// rewrites so a failure part-way through puts the instance back exactly as it
// was. A file that did not exist is recorded as absent and removed on
// rollback.
type connectRestorePoint struct {
	files []connectRestoredFile
}

type connectRestoredFile struct {
	path    string
	data    []byte
	mode    os.FileMode
	existed bool
}

// snapshot records path's current contents. Snapshotting the same path twice
// keeps the first (pre-connect) copy.
func (r *connectRestorePoint) snapshot(path string) error {
	for _, file := range r.files {
		if file.path == path {
			return nil
		}
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		r.files = append(r.files, connectRestoredFile{path: path})
		return nil
	}
	if err != nil {
		return fmt.Errorf("read current contents of %s: %w", path, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read current contents of %s: %w", path, err)
	}
	r.files = append(r.files, connectRestoredFile{path: path, data: data, mode: info.Mode().Perm(), existed: true})
	return nil
}

// rollback restores every snapshotted file and returns the sentence a caller
// appends to its error: empty when connect had written nothing yet, so the
// message never claims a restore that did not happen.
func (r *connectRestorePoint) rollback() string {
	if len(r.files) == 0 {
		return ""
	}
	var failures []string
	for _, file := range r.files {
		var err error
		if file.existed {
			err = os.WriteFile(file.path, file.data, file.mode)
		} else if removeErr := os.Remove(file.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = removeErr
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", file.path, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Sprintf(" (WARNING: could not restore %s — inspect these files by hand)", strings.Join(failures, "; "))
	}
	return " (nothing was left changed: " + instance.ConfigFileName + " and every config/gaggles/*/gaggle.yaml were restored)"
}

// connectForeignProviderRefusal is the CONNECT002 diagnostic: the repos[]
// entry connect would rewrite is not a GitHub entry, so the rewrite would
// silently re-provider a working instance — dropping an Azure DevOps
// project/auth block and pointing the organization at github.com.
func connectForeignProviderRefusal(index int, repo instance.RepoRef) error {
	identity := repo.Owner + "/" + repo.Name
	if repo.Project != "" {
		identity = repo.Owner + "/" + repo.Project + "/" + repo.Name
	}
	guide := ""
	if repo.Provider == "ado" {
		guide = " (docs/guides/ado-authentication.md)"
	}
	return fmt.Errorf("%s repos[%d] declares provider %q (%s) but `goobers connect` writes provider: github entries only; "+
		"rewriting it would drop that entry's project/auth block and point %s at github.com, so every run would fail at "+
		"repository reachability — edit %s by hand%s instead of connecting",
		connectForeignProviderCode, index, repo.Provider, identity, identity, instance.ConfigFileName, guide)
}

// connectRewriteInstanceConfig points cfg's repos[] at the requested
// repository via the standard LoadConfig/WriteConfig round-trip. It reports
// whether cfg changed, and fails closed when nothing carries a placeholder
// and --replace was not given, or when the entry it would rewrite belongs to
// another provider.
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
			if connectForeignProvider(repo) {
				return false, connectForeignProviderRefusal(i, repo)
			}
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
		// Never invite --replace when repos[0] belongs to another provider:
		// that suggestion is exactly how an Azure DevOps instance would be
		// re-providered to github.
		if len(cfg.Repos) > 0 && connectForeignProvider(cfg.Repos[0]) {
			return false, connectForeignProviderRefusal(0, cfg.Repos[0])
		}
		return false, fmt.Errorf(
			"no placeholder repository entry found in %s; currently configured: %s — re-run with --replace to rewrite repos[0]",
			instance.ConfigFileName, describeConfiguredRepos(cfg.Repos),
		)
	}
	if len(cfg.Repos) == 0 {
		cfg.Repos = []instance.RepoRef{target}
		return true, nil
	}
	if connectForeignProvider(cfg.Repos[0]) {
		return false, connectForeignProviderRefusal(0, cfg.Repos[0])
	}
	cfg.Repos[0] = target
	return true, nil
}

// connectForeignProvider reports whether a repos[] entry belongs to a provider
// connect cannot write. An empty provider is the schema default (github) and
// stays connectable.
func connectForeignProvider(repo instance.RepoRef) bool {
	provider := strings.TrimSpace(repo.Provider)
	return provider != "" && provider != "github"
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

// connectLabelSet accumulates label names in insertion order without
// duplicates.
type connectLabelSet struct {
	seen   map[string]bool
	labels []string
}

func (s *connectLabelSet) add(label string) {
	if label = strings.TrimSpace(label); label == "" {
		return
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[label] {
		return
	}
	s.seen[label] = true
	s.labels = append(s.labels, label)
}

func (s *connectLabelSet) addList(value string) {
	for _, label := range splitLabelList(value) {
		s.add(label)
	}
}

func (s *connectLabelSet) has(label string) bool { return s.seen[label] }

func (s *connectLabelSet) sorted() []string {
	labels := append([]string(nil), s.labels...)
	sort.Strings(labels)
	return labels
}

// connectAppliedLabelInputs are the stage inputs whose value names one label a
// built-in WRITES (backloghealth.go's readyLabel, backlogresweep.go's
// resweepReadyLabel), mapped to the label that built-in falls back to when the
// input is omitted.
var connectAppliedLabelInputs = map[string]string{
	"readyLabel":        providers.LabelReady,
	"resweepReadyLabel": providers.LabelReady,
}

// connectExcludedLabelInputs are the stage inputs whose value is a
// comma-separated list of labels a selector reads but never files — they exist
// only because some other stage applies them, so they belong in the ensured
// set and never on the starter issue.
var connectExcludedLabelInputs = []string{"excludeLabels"}

// connectDerivedLabels derives the two label sets a connected repository needs
// before a first run, plus the first connected workflow's name for the
// next-command hint.
//
// selectors are the labels an item must carry for a backlog selector to pick
// it up: the gaggle's spec.backlog.labels, plus every workflow's
// trustLabel/requireLabels inputs (falling back to the gaggle's
// spec.requireLabels default when a workflow declares no requireLabels of its
// own). These, and only these, go on the seeded starter issue.
//
// applied are the labels the same workflows WRITE during a run, plus the ones
// their selectors exclude on: the goobers:claimed claim mirror a claiming
// backlog-query files, each issue-close-out stage's park or status label, the
// readyLabel/resweepReadyLabel a curation stage sets, and every excludeLabels
// entry. GitHub rejects applying a label that does not exist
// (docs/guides/arbitrary-repo-onboarding.md §5), so a first run used to die at
// its first park or close-out even though `connect --seed` reported success
// (cold-start python #7). They are ensured on the repository but never put on
// the starter issue — an issue born goobers:claimed or goobers/status:in-review
// would be excluded by the very selectors meant to find it.
func connectDerivedLabels(set *instance.ConfigSet, owner, name string) (selectors, applied []string, workflow string) {
	selectorSet := &connectLabelSet{}
	appliedSet := &connectLabelSet{}
	for _, gaggle := range set.Gaggles {
		project := gaggle.Spec.Project
		if project.Owner != owner || project.Name != name {
			continue
		}
		// Selectors come from the shared derivation in repolabels.go so the
		// connect-time echo and validate's repository check can never disagree
		// about what this repository is expected to contain.
		for _, label := range repoSelectorLabels(gaggle, set.Workflows) {
			selectorSet.add(label)
		}
		for _, flow := range set.Workflows {
			if flow.Spec.Gaggle != gaggle.Name {
				continue
			}
			if workflow == "" {
				workflow = flow.Name
			}
			for _, task := range flow.Spec.Tasks {
				connectTaskAppliedLabels(task, appliedSet)
			}
		}
	}
	selectors = selectorSet.sorted()
	for _, label := range appliedSet.sorted() {
		if !selectorSet.has(label) {
			applied = append(applied, label)
		}
	}
	return selectors, applied, workflow
}

// connectTaskAppliedLabels adds the labels one task writes or excludes on,
// derived from that task's own inputs and the built-in it invokes.
func connectTaskAppliedLabels(task apiv1.Task, applied *connectLabelSet) {
	for _, key := range connectExcludedLabelInputs {
		applied.addList(task.Inputs[key])
	}
	for key := range connectAppliedLabelInputs {
		if value, ok := task.Inputs[key]; ok {
			applied.add(value)
		}
	}
	subcommand, args := connectBuiltinCommand(task)
	if subcommand == "" {
		return
	}
	if fallback, ok := connectBuiltinLabelDefault(subcommand); ok {
		if _, declared := task.Inputs[fallback.input]; !declared {
			applied.add(fallback.label)
		}
	}
	if subcommand == "issue-close-out" {
		applied.add(connectCloseOutLabel(task.Inputs["status"]))
	}
	// The claim mirror: a claiming backlog-query writes providers.LabelClaimed
	// alongside the ledger lease (runnerwiring.go), so it must exist before
	// the very first claim.
	if connectTaskClaims(subcommand, args, task.PolicyActions) {
		applied.add(providers.LabelClaimed)
	}
}

// connectBuiltinCommand reports the `goobers <subcommand>` a deterministic task
// invokes, using the same shape internal/workflow's compiler matches on.
func connectBuiltinCommand(task apiv1.Task) (string, []string) {
	if task.Run == nil || len(task.Run.Command) < 2 || task.Run.Command[0] != "goobers" {
		return "", nil
	}
	return task.Run.Command[1], task.Run.Command[2:]
}

// connectBuiltinLabelDefault names the label a built-in writes when its label
// input is omitted.
func connectBuiltinLabelDefault(subcommand string) (struct{ input, label string }, bool) {
	switch subcommand {
	case "backlog-health":
		return struct{ input, label string }{"readyLabel", providers.LabelReady}, true
	case "backlog-resweep":
		return struct{ input, label string }{"resweepReadyLabel", providers.LabelReady}, true
	}
	return struct{ input, label string }{}, false
}

// connectTaskClaims reports whether a task claims backlog items, either by
// passing --claim to backlog-query or by declaring the claim policy action.
func connectTaskClaims(subcommand string, args, policyActions []string) bool {
	if subcommand == "backlog-query" {
		for _, arg := range args {
			if arg == "--claim" {
				return true
			}
		}
	}
	for _, action := range policyActions {
		if action == "claim-backlog-items" {
			return true
		}
	}
	return false
}

// connectCloseOutLabel maps an issue-close-out stage's status input to the
// label that stage writes: the two park statuses swap goobers:ready for a
// plain park label (issuecloseout.go), and every other status is mirrored as a
// goobers/status: label by the provider. An omitted status defaults to done,
// exactly as issueCloseOutStatus resolves it.
func connectCloseOutLabel(status string) string {
	switch providers.WorkItemStatus(strings.TrimSpace(status)) {
	case "":
		return providers.StatusLabelFor(providers.WorkItemStatusDone)
	case issueCloseOutNeedsHuman:
		return providers.LabelNeedsHuman
	case issueCloseOutNeedsRemediation:
		return needsRemediationLabel
	default:
		return providers.StatusLabelFor(providers.WorkItemStatus(strings.TrimSpace(status)))
	}
}

// connectSeedCatalog shapes the derived labels and the single starter issue as
// an onboarding seed catalog, so seeding reuses the exact idempotent machinery
// stub-sample ships (EnsureWorkItemLabels, the run-id dedupe marker scan,
// CreateWorkItem). Selector labels are ensured first and are the only ones the
// starter issue carries; the lifecycle labels a run writes are ensured too,
// described for what they are.
func connectSeedCatalog(selectors, applied []string) onboardingSeedCatalog {
	catalogLabels := make([]providers.WorkItemLabel, 0, len(selectors)+len(applied))
	for _, label := range selectors {
		catalogLabels = append(catalogLabels, providers.WorkItemLabel{
			Name:        label,
			Color:       "0E8A16",
			Description: "Eligible for Goobers agentic work (seeded by goobers connect)",
		})
	}
	for _, label := range applied {
		catalogLabels = append(catalogLabels, providers.WorkItemLabel{
			Name:        label,
			Color:       "C5DEF5",
			Description: "Written by Goobers during a run (seeded by goobers connect)",
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
			Labels: selectors,
		}},
	}
}

// connectReportSelectorReality closes the loop a successful connect otherwise
// leaves open: the config is valid and the repository is reachable, but the
// selectors may match nothing that exists (cold-start ado #5). It is a note,
// never a failure — the answer depends on what someone labelled this morning,
// and a provider read that fails must not retract a completed connect.
func connectReportSelectorReality(opts connectOptions, selectors []string, stdout, stderr io.Writer) {
	token := os.Getenv(opts.tokenEnv)
	if token == "" || len(selectors) == 0 {
		return
	}
	lister := newOnboardingIssueSeeder(token)
	if lister == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), stubSampleProviderTimeout)
	defer cancel()
	reality, err := checkRepoSelectorReality(ctx, lister, providers.RepositoryRef{
		Provider: providers.ProviderGitHub,
		Owner:    opts.owner,
		Name:     opts.name,
	}, selectors)
	if err != nil || !reality.Mismatch() {
		return
	}
	note := fmt.Sprintf("note: %s %s, %s\n",
		connectSelectorRealityCode, reality.Summary(opts.owner+"/"+opts.name), reality.Remedy())
	// The JSON envelope is a fixed schema, so machine callers get the note on
	// stderr rather than a field they cannot expect.
	if opts.json {
		pf(stderr, "%s", note)
		return
	}
	pf(stdout, "%s", note)
}

// connectSeedRepository ensures the derived labels exist and files the starter
// issue, degrading exactly like stub-sample when the token env is unset: the
// issue is reported pending and the exit stays 0. It uses the SAME token env
// the connect recorded — deliberately closing the historical
// GOOBERS_GITHUB_ISSUES_TOKEN vs GOOBERS_GITHUB_TOKEN fork.
func connectSeedRepository(opts connectOptions, selectors, applied []string, result *onboardingActionResult, stderr io.Writer) int {
	catalog := connectSeedCatalog(selectors, applied)
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
