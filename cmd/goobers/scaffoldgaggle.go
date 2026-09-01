package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/instance"
)

// `goobers scaffold gaggle` is the missing scaffold-ladder rung the cold-start
// exercise found (swift #1, dotnet #9 — "there is no `goobers scaffold gaggle`
// and no rename path
// (5 fields, 2 files, 1 directory by hand)"). With no --from it creates an
// empty gaggle (directory + gaggle.yaml + manifest registration), matching
// `scaffold goober`/`scaffold workflow`'s additive, never-clobber shape. With
// --from <existing-gaggle> it instead renames that gaggle to the new name —
// the actual pain every cold-start flavor hit: init only ever scaffolds one
// gaggle named "example", and picking a real name means moving a directory
// and editing 2 files by hand. A rename (not a duplicate) is required because
// Goober names are validated instance-globally (api/validate/validate.go's
// dupCheck keys goobers by name alone, unlike Workflow which is keyed by
// (gaggle, name)) — copying "example" alongside a live "example" would
// produce a duplicate-goober error on every goober in the source gaggle.
//
// scaffoldGaggleHelp is this subcommand's own -h body (distinct from the
// shared scaffoldHelp goober/workflow use): its flag set — --force XOR
// --from — differs from theirs, and TestCompletionFlagsMatchHandlerFlagSetsAndSynopsis
// derives each command's expected flags straight from its help text's first
// line, so that line must name exactly this command's real flags.
const scaffoldGaggleHelp = "Usage: goobers scaffold gaggle [--force | --from <existing-gaggle>] <name> [path]\n\n" +
	"Create a new gaggle in the instance at path (an instance root, default\n" +
	"\".\"). With no --from, scaffolds an empty gaggle: gaggle.yaml plus its\n" +
	"manifest.yaml registration, ready for `goobers scaffold goober`/\n" +
	"`scaffold workflow`. Existing files are never replaced unless --force is\n" +
	"set.\n\n" +
	"--from <existing-gaggle> instead renames that gaggle to <name>: it moves\n" +
	"gaggles/<existing-gaggle> to gaggles/<name> and rewrites every identity\n" +
	"reference in place — gaggle.yaml's metadata.name and isolation.namespace,\n" +
	"the manifest.yaml gaggles list entry, and every contained goober/workflow's\n" +
	"spec.gaggle back-reference — while leaving project, backlog, ciCommand,\n" +
	"instructions, and workflow bodies untouched. --force is not accepted\n" +
	"with --from: a rename never overwrites an existing gaggle directory.\n"

func scaffoldGaggleUsage(w io.Writer) {
	pf(w, "%s", scaffoldGaggleHelp)
}

func runScaffoldGaggle(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("scaffold gaggle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	force := fs.Bool("force", false, "replace generated files that already exist (only without --from)")
	from := fs.String("from", "", "rename an existing gaggle to <name>, rewriting its identity fields")
	fs.Usage = func() { scaffoldGaggleUsage(stderr) }
	if err := fs.Parse(reorderScaffoldGaggleArgs(args)); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	name := fs.Arg(0)
	if !scaffoldNamePattern.MatchString(name) {
		pf(stderr, "error: invalid name %q (use lowercase letters, digits, and interior hyphens)\n", name)
		return 2
	}
	path := "."
	if fs.NArg() == 2 {
		path = fs.Arg(1)
	}
	if *from != "" {
		if *force {
			pf(stderr, "error: --force is not accepted with --from (a rename never overwrites an existing gaggle)\n")
			return 2
		}
		if !scaffoldNamePattern.MatchString(*from) {
			pf(stderr, "error: invalid --from name %q\n", *from)
			return 2
		}
		if *from == name {
			pf(stderr, "error: --from %q and the new name %q must differ\n", *from, name)
			return 2
		}
	}

	start, err := filepath.Abs(path)
	if err != nil {
		pf(stderr, "error: resolve path %s: %v\n", path, err)
		return 2
	}
	info, err := os.Stat(start)
	if err != nil {
		pf(stderr, "error: inspect %s: %v\n", start, err)
		return 2
	}
	if !info.IsDir() {
		pf(stderr, "error: %s is not a directory\n", start)
		return 2
	}
	instanceRoot, err := findInstanceRoot(start)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	layout := instance.NewLayout(instanceRoot)
	manifestPath := filepath.Join(layout.ConfigDir(), "manifest.yaml")
	manifest, err := readScaffoldManifest(layout.ConfigDir())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	fromActive := false
	for _, existing := range manifest.Spec.Gaggles {
		if existing == name {
			pf(stderr, "error: gaggle %q is already registered in %s\n", name, manifestPath)
			return 2
		}
		if *from != "" && existing == *from {
			fromActive = true
		}
	}
	if *from != "" && !fromActive {
		pf(stderr, "error: gaggle %q is not active in %s; check the name or scaffold it first\n", *from, manifestPath)
		return 2
	}

	if *from == "" {
		if err := scaffoldNewGaggle(instanceRoot, layout, manifest, manifestPath, name, *force, stdout); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
	} else {
		if err := scaffoldRenameGaggle(layout, manifestPath, *from, name, stdout); err != nil {
			pf(stderr, "error: %v\n", err)
			return 1
		}
	}
	pf(stdout, "next: goobers validate %s\n", instanceRoot)
	return 0
}

// reorderScaffoldGaggleArgs moves --force and --from (with its value) to the
// front so flag.Parse still recognizes them after the positional <name> —
// exactly the shape the task's own example uses
// (`goobers scaffold gaggle ledger --from example`). Plain flag.FlagSet stops
// parsing flags at the first non-flag argument, so without this reordering
// that invocation's --from would land in fs.Args() instead of being parsed.
func reorderScaffoldGaggleArgs(args []string) []string {
	var flags, positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--force" || arg == "-force":
			flags = append(flags, arg)
		case arg == "--from" || arg == "-from":
			flags = append(flags, arg)
			if i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		case strings.HasPrefix(arg, "--from=") || strings.HasPrefix(arg, "-from="):
			flags = append(flags, arg)
		default:
			positionals = append(positionals, arg)
		}
	}
	return append(flags, positionals...)
}

// scaffoldNewGaggle creates an empty gaggle: gaggle.yaml plus its manifest
// registration. It writes no connectionRef: the local runtime resolves every
// access's credential from instance.yaml repos[] by repository identity and
// never consults the field, so seeding one would only earn the scaffolded
// gaggle a REF012 finding (#3296).
func scaffoldNewGaggle(
	instanceRoot string,
	layout instance.Layout,
	manifest *apiv1.Manifest,
	manifestPath, name string,
	force bool,
	stdout io.Writer,
) error {
	gaggleDir := filepath.Join(layout.ConfigDir(), "gaggles", name)
	if err := os.MkdirAll(gaggleDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", gaggleDir, err)
	}
	data := scaffoldTemplateData{Name: name}
	rendered, err := renderScaffoldTemplate("templates/scaffold/gaggle.yaml.tmpl", data)
	if err != nil {
		return fmt.Errorf("render gaggle.yaml: %w", err)
	}
	files := []scaffoldFile{{path: filepath.Join(gaggleDir, "gaggle.yaml"), template: "templates/scaffold/gaggle.yaml.tmpl"}}
	if err := writeScaffoldFiles(instanceRoot, gaggleDir, files, [][]byte{rendered}, force); err != nil {
		return err
	}
	for _, file := range files {
		pf(stdout, "created %s\n", file.path)
	}
	if err := appendManifestGaggle(manifestPath, name); err != nil {
		return err
	}
	pf(stdout, "updated %s\n", manifestPath)
	return nil
}

// scaffoldRenameGaggle moves an existing, manifest-active gaggle directory to
// a new name and rewrites every identity reference the cold-start agents
// otherwise had to find and edit by hand: gaggle.yaml's metadata.name and
// isolation.namespace, every contained Goober/Workflow's spec.gaggle, and the
// manifest.yaml gaggles list entry. Everything else — project, backlog,
// ciCommand, instructions, workflow bodies, skill packages — moves verbatim.
// The caller has already confirmed from is manifest-active and name is not.
func scaffoldRenameGaggle(
	layout instance.Layout,
	manifestPath, from, name string,
	stdout io.Writer,
) error {
	gagglesRoot := filepath.Join(layout.ConfigDir(), "gaggles")
	sourceDir := filepath.Join(gagglesRoot, from)
	sourceInfo, err := os.Lstat(sourceDir)
	if err != nil {
		return fmt.Errorf("inspect gaggle directory %s: %w", sourceDir, err)
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.IsDir() {
		return fmt.Errorf("gaggle directory %s is not a plain directory; refusing to rename through a symlink", sourceDir)
	}
	targetDir := filepath.Join(gagglesRoot, name)
	if _, err := os.Lstat(targetDir); err == nil {
		return fmt.Errorf("refusing to rename onto existing gaggle directory %s", targetDir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", targetDir, err)
	}

	if err := os.Rename(sourceDir, targetDir); err != nil {
		return fmt.Errorf("move %s to %s: %w", sourceDir, targetDir, err)
	}
	pf(stdout, "moved   %s -> %s\n", sourceDir, targetDir)

	var updated []string
	walkErr := filepath.WalkDir(targetDir, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || (!strings.HasSuffix(p, ".yaml") && !strings.HasSuffix(p, ".yml")) {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return fmt.Errorf("read %s: %w", p, readErr)
		}
		rewritten, changed, rewriteErr := rewriteGaggleIdentityYAML(data, name)
		if rewriteErr != nil {
			return fmt.Errorf("rewrite %s: %w", p, rewriteErr)
		}
		if !changed {
			return nil
		}
		if err := os.WriteFile(p, rewritten, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", p, err)
		}
		updated = append(updated, p)
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	sort.Strings(updated)
	for _, p := range updated {
		pf(stdout, "updated %s\n", p)
	}

	if err := renameManifestGaggle(manifestPath, from, name); err != nil {
		return err
	}
	pf(stdout, "updated %s\n", manifestPath)
	return nil
}

// rewriteGaggleIdentityYAML rewrites the one gaggle-identity field a document
// carries — a Gaggle's metadata.name/spec.isolation.namespace, or any other
// kind's spec.gaggle — in place via a comment-preserving yaml.v3 node edit
// (the same technique connect.go's connectRewriteGaggleFile uses via the
// shared yamlMapValue helper), so instructions.md prose, YAML comments, and
// every other field survive untouched. Returns changed=false (and a nil
// slice) for a document with neither shape, e.g. a workflow-agnostic file
// with no spec.gaggle at all.
func rewriteGaggleIdentityYAML(data []byte, name string) ([]byte, bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, false, fmt.Errorf("parse: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, false, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, false, nil
	}

	changed := false
	if kindNode := yamlMapValue(root, "kind"); kindNode != nil && kindNode.Value == "Gaggle" {
		if metadata := yamlMapValue(root, "metadata"); metadata != nil {
			if nameNode := yamlMapValue(metadata, "name"); nameNode != nil && nameNode.Value != name {
				nameNode.Value = name
				changed = true
			}
		}
		if spec := yamlMapValue(root, "spec"); spec != nil {
			if isolation := yamlMapValue(spec, "isolation"); isolation != nil {
				if ns := yamlMapValue(isolation, "namespace"); ns != nil {
					wantNS := "gaggle-" + name
					if ns.Value != wantNS {
						ns.Value = wantNS
						changed = true
					}
				}
			}
		}
	} else if spec := yamlMapValue(root, "spec"); spec != nil {
		if gaggleNode := yamlMapValue(spec, "gaggle"); gaggleNode != nil && gaggleNode.Value != name {
			gaggleNode.Value = name
			changed = true
		}
	}
	if !changed {
		return nil, false, nil
	}
	return encodeYAMLNode(&doc)
}

// renameManifestGaggle replaces the from entry in manifest.yaml's
// spec.gaggles sequence with name in place (same index, comments preserved)
// rather than removing and re-appending, so an intentional ordering survives.
func renameManifestGaggle(manifestPath, from, name string) error {
	doc, err := readYAMLNodeDocument(manifestPath)
	if err != nil {
		return err
	}
	root := doc.Content[0]
	spec := yamlMapValue(root, "spec")
	if spec == nil {
		return fmt.Errorf("%s: no spec mapping", manifestPath)
	}
	gaggles := yamlMapValue(spec, "gaggles")
	if gaggles == nil {
		return fmt.Errorf("%s: no spec.gaggles list", manifestPath)
	}
	found := false
	for _, item := range gaggles.Content {
		if item.Value == from {
			item.Value = name
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%s: spec.gaggles does not list %q", manifestPath, from)
	}
	rendered, _, err := encodeYAMLNode(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, rendered, 0o644)
}

// appendManifestGaggle adds name to manifest.yaml's spec.gaggles sequence,
// creating the sequence if the manifest declared none.
func appendManifestGaggle(manifestPath, name string) error {
	doc, err := readYAMLNodeDocument(manifestPath)
	if err != nil {
		return err
	}
	root := doc.Content[0]
	spec := yamlMapValue(root, "spec")
	if spec == nil {
		return fmt.Errorf("%s: no spec mapping", manifestPath)
	}
	gaggles := yamlMapValue(spec, "gaggles")
	if gaggles == nil {
		gaggles = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		spec.Content = append(spec.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "gaggles"}, gaggles)
	}
	for _, item := range gaggles.Content {
		if item.Value == name {
			return nil
		}
	}
	gaggles.Content = append(gaggles.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: name})
	rendered, _, err := encodeYAMLNode(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, rendered, 0o644)
}

func readYAMLNodeDocument(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("parse %s: not a YAML document", path)
	}
	return &doc, nil
}

func encodeYAMLNode(doc *yaml.Node) ([]byte, bool, error) {
	var out strings.Builder
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(doc); err != nil {
		return nil, false, fmt.Errorf("encode: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, false, fmt.Errorf("encode: %w", err)
	}
	return []byte(out.String()), true, nil
}
