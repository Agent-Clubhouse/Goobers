package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/goobers/goobers/internal/gaggletemplate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/platform/lock"
)

const templatesHelp = `Usage: goobers config templates <import|update|backprop|check|status> [flags] [instance-root]

Manage opt-in, repository-backed gaggle copies. Existing gaggles are unchanged.
Imports start disabled. Updates are explicit and never overwrite conflicting
local edits. Backprop writes a reviewable user's config checkout, never the
template repository, and does not commit or push.
See docs/guides/gaggle-templates.md for package authoring and recovery.
`

const templateImportHelp = `Usage: goobers config templates import --repository <repo> --directory <path> --gaggle <name> [--ref <branch>] [--token-env <name>] [--source <config-root>] [instance-root]

Import a self-contained gaggle from a committed local Git repository or HTTPS
repository. The ref is a branch (default main); directory is repository-relative.
HTTPS sources require a read-only token environment variable. No hooks run.
Writes ordinary definitions plus .template/source.yaml and lock.json to the
user's config source, and registers the gaggle in its manifest. Starts disabled.
--source defaults to a local workflowSource, otherwise the instance config/.
Stop the daemon first. Review, commit, and deploy the user's source afterwards.
`

const templateUpdateHelp = `Usage: goobers config templates update --gaggle <name> [--source <config-root>] [instance-root]

Fetch the tracked template branch and three-way merge the previous pristine
template, local customizations, and updated template. Conflicts or validation
errors leave the accepted files and lock unchanged. No automatic update.
Stop the daemon first. Review, commit, and deploy the user's source afterwards.
`

const templateBackpropHelp = `Usage: goobers config templates backprop --gaggle <name> [--source <config-root>] [instance-root]

Persist runtime edits into the user's config checkout using the recorded
deployment baseline. --source defaults to a local workflowSource; it must be
separate from the runtime instance. Source changes merge or report conflicts.
Stop the daemon first. No commit or push is performed: review and commit the
reported files in YOUR repository. The shared template is never modified.
`

const templateCheckHelp = `Usage: goobers config templates check [instance-root]

Check enrolled runtime gaggles for template changes and persist notify-only
status. Fetches source branches but never changes definitions or accepted pins.
Reports changed files, merge conflicts, pending backprop and check errors.
Exit 0 means checks succeeded (updates may be available); 1 means a check failed.
`

const templateStatusHelp = `Usage: goobers config templates status [instance-root]

Show cached template update state without network access. No prior check or a
failed/stale check is not reported as up to date. Unenrolled gaggles are omitted.
`

func runTemplates(args []string, stdout, stderr io.Writer) int {
	pf(stdout, "%s", templatesHelp)
	if len(args) > 0 && args[0] != "-h" && args[0] != "--help" {
		return 2
	}
	return 0
}

func runTemplateImport(args []string, stdout, stderr io.Writer) int {
	return runTemplateMutation("import", args, stdout, stderr)
}

func runTemplateUpdate(args []string, stdout, stderr io.Writer) int {
	return runTemplateMutation("update", args, stdout, stderr)
}

func runTemplateBackprop(args []string, stdout, stderr io.Writer) int {
	return runTemplateMutation("backprop", args, stdout, stderr)
}

type templateOptions struct {
	repository, directory, ref, tokenEnv, gaggle, source string
}

func runTemplateMutation(action string, args []string, stdout, stderr io.Writer) int {
	flags := newCLIFlagSet("config templates "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	var opts templateOptions
	flags.StringVar(&opts.gaggle, "gaggle", "", "destination gaggle name")
	flags.StringVar(&opts.source, "source", "", "user's writable config source root")
	if action == "import" {
		flags.StringVar(&opts.repository, "repository", "", "template Git repository path or HTTPS URL")
		flags.StringVar(&opts.directory, "directory", "", "repository-relative self-contained gaggle directory")
		flags.StringVar(&opts.ref, "ref", "main", "template branch")
		flags.StringVar(&opts.tokenEnv, "token-env", "", "read-only template token environment variable")
	}
	flags.Usage = helpUsage(stderr, "config templates "+action)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 1 || !scaffoldNamePattern.MatchString(opts.gaggle) {
		flags.Usage()
		return 2
	}
	root := "."
	if flags.NArg() == 1 {
		root = flags.Arg(0)
	}
	if err := executeTemplateMutation(action, root, opts, stdout, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func executeTemplateMutation(action, root string, opts templateOptions, stdout, stderr io.Writer) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	layout := instance.NewLayout(root)
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return err
	}
	if err := prepareManualRoot(layout, stderr); err != nil {
		return err
	}
	if err := os.MkdirAll(layout.SchedulerDir(), 0755); err != nil {
		return err
	}
	held, err := lock.TryAcquire(filepath.Join(layout.SchedulerDir(), "up.lock"))
	if err != nil {
		return fmt.Errorf("template mutations require an offline instance; run goobers down first: %w", err)
	}
	defer func() { _ = held.Release() }()
	source, err := writableTemplateConfig(root, cfg, opts.source)
	if err != nil {
		return err
	}
	sourceLock, err := lock.TryAcquire(filepath.Join(source, ".template-edit.lock"))
	if err != nil {
		return err
	}
	defer func() { _ = sourceLock.Release() }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch action {
	case "import":
		err = importTemplate(ctx, root, source, opts)
	case "update":
		err = updateTemplate(ctx, root, source, opts.gaggle)
	case "backprop":
		err = backpropTemplate(root, source, opts.gaggle)
	}
	if err != nil {
		return err
	}
	pf(stdout, "template %s complete: %s\nReview and commit the changes in your config repository; no commit or push was performed.\n",
		action, filepath.Join(source, "gaggles", opts.gaggle))
	return nil
}

func writableTemplateConfig(root string, cfg *instance.Config, explicit string) (string, error) {
	source := explicit
	if source == "" && cfg.WorkflowSource != nil {
		if cfg.WorkflowSource.Path == "" {
			return "", errors.New("remote workflowSource requires --source pointing to your writable config checkout")
		}
		source = cfg.WorkflowSource.Path
	}
	if source == "" {
		source = instance.NewLayout(root).ConfigDir()
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return "", err
	}
	if _, report, err := instance.LoadConfigDir(source); err != nil {
		return "", fmt.Errorf("user config source %s is invalid: %w; %s", source, err, validationIssueSummary(report))
	}
	return source, nil
}

var resolveGaggleTemplate = func(ctx context.Context, root string, source gaggletemplate.Source, priorRevision string) (gaggletemplate.Tree, string, error) {
	if err := source.Validate(); err != nil {
		return nil, "", err
	}
	config := instance.WorkflowSource{Kind: instance.WorkflowSourceKindGit, Ref: source.Ref}
	if strings.HasPrefix(source.Repository, "https://") {
		config.URL = source.Repository
		config.Token = &instance.TokenRef{Env: source.TokenEnv}
	} else {
		if source.TokenEnv != "" {
			return nil, "", errors.New("local template repositories must not configure tokenEnv")
		}
		config.Path = source.Repository
	}
	scrubber := journal.NewRegistryScrubber()
	gitSource, err := instance.NewWorkflowGitSource(root, config, nil, scrubber, nil)
	if err != nil {
		return nil, "", err
	}
	snapshot, err := gitSource.Resolve(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("resolve template: %s", scrubber.Scrub([]byte(err.Error())))
	}
	if err := gitSource.RequireAncestor(ctx, priorRevision, filepath.Base(snapshot)); err != nil {
		return nil, "", err
	}
	if warnings := gitSource.Warnings(); len(warnings) != 0 {
		return nil, "", fmt.Errorf("template snapshot cannot preserve repository links: %s", strings.Join(warnings, "; "))
	}
	directory := snapshot
	for _, component := range strings.Split(source.Directory, "/") {
		directory = filepath.Join(directory, component)
		info, err := os.Lstat(directory)
		if err != nil {
			return nil, "", err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, "", errors.New("template directory must not traverse links")
		}
	}
	tree, err := gaggletemplate.ReadTree(directory)
	if err != nil {
		return nil, "", err
	}
	bound, err := gaggletemplate.Bind(tree, source.Gaggle)
	if err == nil {
		err = validateTemplatePackage(source.Gaggle, bound)
	}
	return bound, filepath.Base(snapshot), err
}

func importTemplate(ctx context.Context, root, configDir string, opts templateOptions) error {
	if opts.repository == "" || opts.directory == "" {
		return errors.New("import requires --repository and --directory")
	}
	source := gaggletemplate.Source{
		SchemaVersion: 1, Repository: opts.repository, Ref: opts.ref,
		Directory: filepath.ToSlash(opts.directory), Gaggle: opts.gaggle, TokenEnv: opts.tokenEnv,
	}
	if !strings.HasPrefix(source.Repository, "https://") {
		absolute, err := filepath.Abs(source.Repository)
		if err != nil {
			return err
		}
		source.Repository = absolute
	}
	target := filepath.Join(configDir, "gaggles", opts.gaggle)
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("import destination already exists or is inaccessible: %s", target)
	}
	tree, revision, err := resolveGaggleTemplate(ctx, root, source, "")
	if err != nil {
		return err
	}
	manifestPath := filepath.Join(configDir, "manifest.yaml")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	manifest, err := templateManifest(raw, opts.gaggle)
	if err != nil {
		return err
	}
	stage, err := stageTemplate(configDir, opts.gaggle, tree, source, revision, tree, manifest)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	sourceInfo, err := os.Stat(configDir)
	if err != nil {
		return err
	}
	runtimeInfo, err := os.Stat(instance.NewLayout(root).ConfigDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if runtimeInfo != nil && os.SameFile(sourceInfo, runtimeInfo) {
		if err := gaggletemplate.RecordDeployment(stage); err != nil {
			return err
		}
	}
	if err := gaggletemplate.Publish(target, stage, nil); err != nil {
		return err
	}
	if err := writeWorkflowSourceAtomically(manifestPath, manifest, 0644); err != nil {
		return fmt.Errorf("imported disabled gaggle but could not register it; restore manifest or register %s before deploying: %w", opts.gaggle, err)
	}
	return nil
}

func templateManifest(raw []byte, name string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) != 1 {
		return nil, errors.New("manifest must contain one document")
	}
	spec := mappingValue(doc.Content[0], "spec")
	if spec == nil {
		return nil, errors.New("manifest has no spec")
	}
	gaggles := mappingValue(spec, "gaggles")
	if gaggles == nil {
		gaggles = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		spec.Content = append(spec.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "gaggles"}, gaggles)
	}
	for _, item := range gaggles.Content {
		if item.Value == name {
			return nil, fmt.Errorf("gaggle %s is already registered", name)
		}
	}
	gaggles.Content = append(gaggles.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name})
	return yaml.Marshal(&doc)
}

func updateTemplate(ctx context.Context, root, configDir, name string) error {
	if err := instance.CheckGuidedSourceInstancePaths(root, configDir); err != nil {
		return fmt.Errorf("update requires a separate user config source; persist runtime edits with backprop first: %w", err)
	}
	target := filepath.Join(configDir, "gaggles", name)
	tracking, err := requiredTemplate(target)
	if err != nil {
		return err
	}
	local, err := gaggletemplate.ReadTree(target)
	if err != nil {
		return err
	}
	upstream, revision, err := resolveGaggleTemplate(ctx, root, tracking.Source, tracking.Lock.Revision)
	if err != nil {
		return err
	}
	merged, conflicts, err := gaggletemplate.Merge(tracking.Lock.Baseline, local, upstream)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("template conflicts against revision %s (nothing changed): %s", revision, strings.Join(conflicts, "; "))
	}
	stage, err := stageTemplate(configDir, name, merged, tracking.Source, revision, upstream, nil)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	return gaggletemplate.Publish(target, stage, local)
}

func requiredTemplate(root string) (*gaggletemplate.Tracking, error) {
	tracking, err := gaggletemplate.Load(root)
	if err != nil {
		return nil, err
	}
	if tracking == nil {
		return nil, errors.New("gaggle is not enrolled in template tracking")
	}
	return tracking, nil
}

func backpropTemplate(root, sourceDir, name string) error {
	runtimeDir := filepath.Join(instance.NewLayout(root).ConfigDir(), "gaggles", name)
	target := filepath.Join(sourceDir, "gaggles", name)
	if err := instance.CheckGuidedSourceInstancePaths(root, sourceDir); err != nil {
		return err
	}
	runtimeTracking, err := requiredTemplate(runtimeDir)
	if err != nil {
		return err
	}
	sourceTracking, err := requiredTemplate(target)
	if err != nil {
		return err
	}
	if runtimeTracking.Source != sourceTracking.Source {
		return errors.New("source checkout and runtime template provenance differ")
	}
	base, err := gaggletemplate.Deployment(runtimeDir)
	if err != nil {
		return err
	}
	runtime, err := gaggletemplate.ReadTree(runtimeDir)
	if err != nil {
		return err
	}
	source, err := gaggletemplate.ReadTree(target)
	if err != nil {
		return err
	}
	merged, conflicts, err := gaggletemplate.Merge(base, runtime, source)
	if err != nil {
		return err
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("backprop conflicts (nothing changed): %s", strings.Join(conflicts, "; "))
	}
	stage, err := stageTemplate(sourceDir, name, merged, sourceTracking.Source,
		sourceTracking.Lock.Revision, sourceTracking.Lock.Baseline, nil)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	if err := gaggletemplate.Publish(target, stage, source); err != nil {
		return err
	}
	// Do not change the runtime or its deployed baseline before the user's
	// reviewed source is deployed. A repeat capture is idempotent.
	return nil
}
