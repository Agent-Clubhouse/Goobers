// Package validate lints a Goobers config-as-code directory (and individual
// runtime envelopes) against the canonical JSON Schemas and the cross-object
// reference rules from the specs. It is consumed by the `validate` CLI and by
// the operator's admission path.
package validate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/configboundary"
	"github.com/goobers/goobers/internal/gooberassets"
	wf "github.com/goobers/goobers/internal/workflow"
)

// Severity ranks an issue.
type Severity string

const (
	// Error fails validation (non-zero exit).
	Error Severity = "error"
	// Warning is reported but does not fail validation.
	Warning Severity = "warning"
)

// WarningCode is a stable machine-readable identifier for a validation finding.
type WarningCode string

const (
	// WarningDeprecatedFeature identifies use of a deprecated DSL feature.
	WarningDeprecatedFeature WarningCode = "VER001"
	// WarningPreviewFeature identifies use of a preview DSL feature.
	WarningPreviewFeature WarningCode = "VER002"
	// WarningCompatibility identifies a compatibility notice.
	WarningCompatibility WarningCode = "VER003"
	// ErrorRemovedFeature identifies use of a removed DSL feature.
	ErrorRemovedFeature WarningCode = "VER004"
	// WarningModelFallback identifies fallback from a requested model.
	WarningModelFallback WarningCode = "MODEL002"
)

// Issue is a single validation finding.
type Issue struct {
	Code     WarningCode `json:"code,omitempty"`
	Severity Severity    `json:"severity"`
	File     string      `json:"file,omitempty"`
	Kind     string      `json:"kind,omitempty"`
	Name     string      `json:"name,omitempty"`
	Gaggle   string      `json:"gaggle,omitempty"`
	Message  string      `json:"message"`
}

func (i Issue) String() string {
	code := ""
	if i.Code != "" {
		code = " " + string(i.Code)
	}
	return fmt.Sprintf("%-7s%s %s: %s", strings.ToUpper(string(i.Severity)), code, i.Scope(), i.Message)
}

// CLIString preserves the validator's established text representation while
// structured consumers use the richer warning provenance.
func (i Issue) CLIString() string {
	return i.cliIssue().String()
}

func (i Issue) cliIssue() Issue {
	if i.Severity == Warning && i.Code == WarningCompatibility && i.Gaggle != "" && i.Kind == "Workflow" {
		i.Code = ""
		i.File = ""
		i.Gaggle = ""
	}
	return i
}

// Scope returns the issue's stable human and machine-readable location.
func (i Issue) Scope() string {
	object := ""
	if i.Kind != "" {
		object = i.Kind
		if i.Name != "" {
			object += "/" + i.Name
		}
	}
	if i.Gaggle != "" {
		object = "Gaggle/" + i.Gaggle + " " + object
	}
	switch {
	case i.File != "" && object != "":
		return i.File + " " + object
	case i.File != "":
		return i.File
	case object != "":
		return object
	default:
		return "config"
	}
}

// CodedWarning is the stable warning shape projected by CLI and API consumers.
type CodedWarning struct {
	Code        WarningCode `json:"code"`
	Severity    Severity    `json:"severity"`
	Scope       string      `json:"scope"`
	Explanation string      `json:"explanation"`
}

func (w CodedWarning) String() string {
	return fmt.Sprintf("%s %s %s: %s", strings.ToUpper(string(w.Severity)), w.Code, w.Scope, w.Explanation)
}

// Report is the result of validating a directory.
type Report struct {
	Issues  []Issue `json:"issues"`
	Files   int     `json:"files"`
	Objects int     `json:"objects"`
}

// HasErrors reports whether any error-severity issue was found.
func (r *Report) HasErrors() bool {
	for _, i := range r.Issues {
		if i.Severity == Error {
			return true
		}
	}
	return false
}

// Warnings returns coded warnings in deterministic scope, code, explanation order.
func (r *Report) Warnings() []CodedWarning {
	warnings := make([]CodedWarning, 0)
	if r == nil {
		return warnings
	}
	for _, issue := range r.Issues {
		if issue.Severity != Warning {
			continue
		}
		warnings = append(warnings, CodedWarning{
			Code:        issue.Code,
			Severity:    issue.Severity,
			Scope:       issue.Scope(),
			Explanation: issue.Message,
		})
	}
	sort.Slice(warnings, func(i, j int) bool {
		if warnings[i].Scope != warnings[j].Scope {
			return warnings[i].Scope < warnings[j].Scope
		}
		if warnings[i].Code != warnings[j].Code {
			return warnings[i].Code < warnings[j].Code
		}
		return warnings[i].Explanation < warnings[j].Explanation
	})
	return warnings
}

// CLIWarnings returns warnings in the representation used before workflow
// warnings gained API-only code and provenance fields.
func (r *Report) CLIWarnings() []CodedWarning {
	if r == nil {
		return nil
	}
	return r.CLIReport().Warnings()
}

// CLIReport preserves the validator's established JSON representation while
// structured API consumers use the richer warning provenance.
func (r *Report) CLIReport() *Report {
	if r == nil {
		return nil
	}
	report := &Report{
		Files:   r.Files,
		Objects: r.Objects,
	}
	if r.Issues != nil {
		report.Issues = make([]Issue, 0, len(r.Issues))
	}
	for _, issue := range r.Issues {
		report.Issues = append(report.Issues, issue.cliIssue())
	}
	return report
}

func (r *Report) add(sev Severity, file, kind, name, format string, args ...interface{}) {
	r.addCoded("", sev, file, kind, name, format, args...)
}

func (r *Report) addCoded(code WarningCode, sev Severity, file, kind, name, format string, args ...interface{}) {
	r.Issues = append(r.Issues, Issue{
		Code:     code,
		Severity: sev,
		File:     file,
		Kind:     kind,
		Name:     name,
		Message:  fmt.Sprintf(format, args...),
	})
}

func (r *Report) addWarning(code WarningCode, file, gaggle, kind, name, format string, args ...interface{}) {
	r.Issues = append(r.Issues, Issue{
		Code:     code,
		Severity: Warning,
		File:     file,
		Gaggle:   gaggle,
		Kind:     kind,
		Name:     name,
		Message:  fmt.Sprintf(format, args...),
	})
}

func (r *Report) addFeatureDiagnostics(file, gaggle, kind, name string, diagnostics []wf.FeatureDiagnostic) {
	for _, diagnostic := range diagnostics {
		severity := Warning
		if diagnostic.Blocking {
			severity = Error
		}
		var code WarningCode
		switch diagnostic.Feature.Level {
		case wf.SupportDeprecated:
			code = WarningDeprecatedFeature
		case wf.SupportPreview:
			code = WarningPreviewFeature
		case wf.SupportRemoved:
			code = ErrorRemovedFeature
		}
		r.Issues = append(r.Issues, Issue{
			Code:     code,
			Severity: severity,
			File:     file,
			Gaggle:   gaggle,
			Kind:     kind,
			Name:     name,
			Message:  diagnostic.Message,
		})
	}
}

// Validator holds compiled schemas, reusable across many validations.
type Validator struct {
	compiler *jsonschema.Compiler
	cache    map[string]*jsonschema.Schema
}

// New builds a Validator with all embedded schemas registered so cross-schema
// $refs (e.g. invocation -> result) resolve.
func New() (*Validator, error) {
	c := jsonschema.NewCompiler()
	c.Draft = jsonschema.Draft2020
	for _, f := range schemas.Files() {
		data, err := schemas.FS.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("read embedded schema %s: %w", f, err)
		}
		if err := c.AddResource(schemas.BaseURI+f, bytes.NewReader(data)); err != nil {
			return nil, fmt.Errorf("add schema %s: %w", f, err)
		}
	}
	return &Validator{compiler: c, cache: map[string]*jsonschema.Schema{}}, nil
}

func (v *Validator) schema(file string) (*jsonschema.Schema, error) {
	if s, ok := v.cache[file]; ok {
		return s, nil
	}
	s, err := v.compiler.Compile(schemas.BaseURI + file)
	if err != nil {
		return nil, err
	}
	v.cache[file] = s
	return s, nil
}

// ValidateJSON validates raw JSON bytes against the named schema file.
func (v *Validator) ValidateJSON(schemaFile string, jsonBytes []byte) error {
	s, err := v.schema(schemaFile)
	if err != nil {
		return err
	}
	var doc interface{}
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		return fmt.Errorf("parse json: %w", err)
	}
	return s.Validate(doc)
}

// ValidateEnvelope validates a JSON envelope ("invocation"|"result"|"verdict").
func (v *Validator) ValidateEnvelope(name string, jsonBytes []byte) error {
	file, ok := schemas.Envelope[name]
	if !ok {
		return fmt.Errorf("unknown envelope %q", name)
	}
	return v.ValidateJSON(file, jsonBytes)
}

var docSep = regexp.MustCompile(`(?m)^---\s*$`)

// typeMeta is the minimal shape needed to dispatch a document to its schema.
type typeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

// loadedDoc is one parsed YAML document plus provenance.
type loadedDoc struct {
	file string
	dir  string
	kind string
	name string
	json []byte
}

// ValidateDir validates every YAML object under root: schema-checks each, then
// applies cross-object reference rules. The returned Report is always non-nil.
func (v *Validator) ValidateDir(root string) (*Report, error) {
	r := &Report{}
	var docs []loadedDoc

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if gooberassets.IsSourceDir(path) {
			if assetErr := gooberassets.Validate(path); assetErr != nil {
				rel, _ := filepath.Rel(root, path)
				r.add(Error, filepath.ToSlash(rel), "", "", "invalid goober assets: %v", assetErr)
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		r.Files++
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, seg := range docSep.Split(string(raw), -1) {
			if strings.TrimSpace(seg) == "" {
				continue
			}
			jb, err := yaml.YAMLToJSON([]byte(seg))
			if err != nil {
				r.add(Error, rel, "", "", "invalid YAML: %v", err)
				continue
			}
			var tm typeMeta
			if err := json.Unmarshal(jb, &tm); err != nil || tm.Kind == "" {
				r.add(Error, rel, "", "", "document is missing apiVersion/kind")
				continue
			}
			docs = append(docs, loadedDoc{
				file: rel, dir: filepath.Dir(path), kind: tm.Kind, name: tm.Metadata.Name, json: jb,
			})
		}
		return nil
	})
	if err != nil {
		return r, fmt.Errorf("walk %s: %w", root, err)
	}

	idx := newIndex()
	for _, doc := range docs {
		r.Objects++
		if doc.kind == "Manifest" {
			idx.manifestDocsSeen++
		}
		schemaFile, ok := schemas.Kind[doc.kind]
		if !ok {
			r.add(Error, doc.file, doc.kind, doc.name, "unknown kind %q", doc.kind)
			continue
		}
		if err := v.ValidateJSON(schemaFile, doc.json); err != nil {
			for _, line := range flattenSchemaError(err) {
				r.add(Error, doc.file, doc.kind, doc.name, "%s", line)
			}
		}
		// Index the object even when it failed schema validation. Most schema
		// violations (bad enum, missing field, an extra evaluator block) still
		// decode cleanly, and keeping the object in the index lets the semantic
		// cross-ref checks run anyway. That (a) surfaces the clearer field-level
		// messages (e.g. the GT-016 "exactly one evaluator block" message, which a
		// raw JSON-Schema `not` failure renders only as "not failed"), and (b)
		// avoids dropping the object — which would dangle every reference to it and
		// blame the wrong object with a misleading cascade. If the object cannot be
		// decoded into its typed form, idx.add reports that and skips it.
		idx.add(r, doc)
	}

	idx.crossCheck(r)
	sortIssues(r)
	return r, nil
}

func sortIssues(r *Report) {
	sort.SliceStable(r.Issues, func(a, b int) bool {
		if r.Issues[a].File != r.Issues[b].File {
			return r.Issues[a].File < r.Issues[b].File
		}
		return r.Issues[a].Message < r.Issues[b].Message
	})
}

// flattenSchemaError turns a jsonschema ValidationError tree into readable lines.
func flattenSchemaError(err error) []string {
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []string{err.Error()}
	}
	var lines []string
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			loc := e.InstanceLocation
			if loc == "" {
				loc = "(root)"
			}
			lines = append(lines, fmt.Sprintf("%s: %s", loc, friendlySchemaMessage(e.Message)))
			return
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	if len(lines) == 0 {
		lines = append(lines, ve.Message)
	}
	return lines
}

// friendlySchemaMessage rewrites a few terse JSON-Schema keyword messages into
// text that points at the actual problem. The raw library renders a failed
// `not`/`oneOf` as just "not failed"/"oneOf failed", which is opaque; for these
// the accompanying semantic cross-ref message (when one exists) carries the real
// explanation, and this makes the schema line itself less cryptic.
func friendlySchemaMessage(msg string) string {
	switch {
	case msg == "not failed":
		return "value violates an exclusivity constraint (a mutually-exclusive or forbidden field combination is present)"
	case strings.HasPrefix(msg, "oneOf failed"):
		return "value must match exactly one of the allowed shapes (" + msg + ")"
	default:
		return msg
	}
}

type workflowIdentity struct {
	gaggle string
	name   string
}

type indexedWorkflow struct {
	definition apiv1.Workflow
	file       string
}

// index holds the typed objects keyed by their config identities for
// cross-reference checks.
type index struct {
	manifests    []apiv1.Manifest
	gaggles      map[string]apiv1.Gaggle
	goobers      map[string]apiv1.Goober
	workflows    map[workflowIdentity]indexedWorkflow
	manifestFile map[string]string
	gooberFile   map[string]string
	gooberDir    map[string]string // goober name -> source dir (for instruction path checks)
	gaggleFile   map[string]string // gaggle name -> source file (for connection-ref checks)

	// manifestDocsSeen counts documents with kind=Manifest regardless of whether
	// they passed schema validation, so we don't double-report "no Manifest" for
	// a manifest that merely failed its schema.
	manifestDocsSeen int
}

func newIndex() *index {
	return &index{
		gaggles:      map[string]apiv1.Gaggle{},
		goobers:      map[string]apiv1.Goober{},
		workflows:    map[workflowIdentity]indexedWorkflow{},
		manifestFile: map[string]string{},
		gooberFile:   map[string]string{},
		gooberDir:    map[string]string{},
		gaggleFile:   map[string]string{},
	}
}

func (ix *index) add(r *Report, doc loadedDoc) {
	switch doc.kind {
	case "Manifest":
		var m apiv1.Manifest
		if err := yaml.Unmarshal(doc.json, &m); err != nil {
			r.add(Error, doc.file, doc.kind, doc.name, "decode: %v", err)
			return
		}
		ix.manifests = append(ix.manifests, m)
		ix.manifestFile[m.Name] = doc.file
	case "Gaggle":
		var g apiv1.Gaggle
		if err := yaml.Unmarshal(doc.json, &g); err != nil {
			r.add(Error, doc.file, doc.kind, doc.name, "decode: %v", err)
			return
		}
		ix.dupCheck(r, doc, "Gaggle", g.Name, func() bool { _, ok := ix.gaggles[g.Name]; return ok })
		ix.gaggles[g.Name] = g
		ix.gaggleFile[g.Name] = doc.file
	case "Goober":
		var g apiv1.Goober
		if err := yaml.Unmarshal(doc.json, &g); err != nil {
			r.add(Error, doc.file, doc.kind, doc.name, "decode: %v", err)
			return
		}
		ix.dupCheck(r, doc, "Goober", g.Name, func() bool { _, ok := ix.goobers[g.Name]; return ok })
		ix.goobers[g.Name] = g
		ix.gooberFile[g.Name] = doc.file
		ix.gooberDir[g.Name] = doc.dir
	case "Workflow":
		var w apiv1.Workflow
		if err := yaml.Unmarshal(doc.json, &w); err != nil {
			r.add(Error, doc.file, doc.kind, doc.name, "decode: %v", err)
			return
		}
		identity := workflowIdentity{gaggle: w.Spec.Gaggle, name: w.Name}
		ix.dupCheck(r, doc, "Workflow", w.Name, func() bool {
			_, ok := ix.workflows[identity]
			return ok
		})
		ix.workflows[identity] = indexedWorkflow{definition: w, file: doc.file}
	}
}

func (ix *index) dupCheck(r *Report, doc loadedDoc, kind, name string, exists func() bool) {
	if exists() {
		r.add(Error, doc.file, kind, name, "duplicate %s name %q", kind, name)
	}
}

// crossCheck applies the spec's reference rules across all loaded objects.
func (ix *index) crossCheck(r *Report) {
	if len(ix.manifests) == 0 && ix.manifestDocsSeen == 0 {
		r.add(Error, "", "Manifest", "", "no Manifest object found in config directory")
	}
	if len(ix.manifests) > 1 {
		// Error, not Warning (#243): internal/instance/configdir.go and
		// internal/configsync/loader.go both reject a config directory with
		// more than one Manifest outright — a validate-only consumer must
		// not report success-with-warning for a config the daemon actually
		// refuses to load.
		r.add(Error, "", "Manifest", "", "more than one Manifest found (%d); exactly one is expected", len(ix.manifests))
	}
	allowPreview := ix.allowPreviewFeatures(r)

	// Manifest -> gaggle references resolve.
	for _, m := range ix.manifests {
		for _, gname := range m.Spec.Gaggles {
			if _, ok := ix.gaggles[gname]; !ok {
				r.add(Error, ix.manifestFile[m.Name], "Manifest", m.Name,
					"spec.gaggles references %q, but no Gaggle/%s definition was found", gname, gname)
			}
		}
	}
	// Gaggle -> Connection references resolve (MGV-4, #1011). A foreign gaggle
	// routes its repo/backlog credentials through a named Manifest Connection;
	// a connectionRef that names no declared Connection is a half-configured
	// gaggle that fails confusingly at runtime (an unresolved credential),
	// so catch it here with a message naming the gaggle, the field, and the
	// missing connection. An empty connectionRef is left alone: at local tiers
	// a gaggle legitimately binds its repo token per-repo in instance.yaml
	// rather than through a Manifest Connection.
	ix.checkGaggleConnections(r)
	// Gaggle CI-command coherence (MGV-4) over #1009's ciCommand surface.
	ix.checkGaggleCICommand(r)
	// Gaggle branch-prefix coherence (MGV-4) over #965/#1010's branchNamespace surface.
	ix.checkGaggleBranchNamespace(r)
	// Goober -> gaggle / workflow references resolve; instruction file exists.
	for _, g := range ix.goobers {
		file := ix.gooberFile[g.Name]
		r.addFeatureDiagnostics(file, g.Spec.Gaggle, "Goober", g.Name,
			wf.CheckGooberFeatureSupport(g.Spec, allowPreview))
		if _, ok := ix.gaggles[g.Spec.Gaggle]; !ok {
			r.add(Error, file, "Goober", g.Name, "spec.gaggle names %q, but no Gaggle/%s definition was found",
				g.Spec.Gaggle, g.Spec.Gaggle)
		}
		for _, wf := range g.Spec.Workflows {
			identity := workflowIdentity{gaggle: g.Spec.Gaggle, name: wf}
			if _, ok := ix.workflows[identity]; !ok {
				r.add(Error, file, "Goober", g.Name,
					"spec.workflows references %q, but no Workflow/%s is defined in gaggle %q",
					wf, wf, g.Spec.Gaggle)
			}
		}
		for _, value := range g.Spec.Capabilities {
			if capability.Known(value) {
				continue
			}
			message := fmt.Sprintf("spec.capabilities contains unknown capability %q", value)
			if suggestion, ok := capability.Suggest(value); ok {
				message += fmt.Sprintf("; did you mean %q?", suggestion)
			}
			r.add(Error, file, "Goober", g.Name, "%s", message)
		}
		if g.Spec.Instructions != "" {
			p := filepath.Join(ix.gooberDir[g.Name], g.Spec.Instructions)
			info, err := os.Stat(p)
			expected := filepath.ToSlash(filepath.Join(filepath.Dir(file), g.Spec.Instructions))
			switch {
			case errors.Is(err, fs.ErrNotExist):
				r.add(Error, file, "Goober", g.Name,
					"spec.instructions file %q was not found; expected it at %q", g.Spec.Instructions, expected)
			case err != nil:
				r.add(Error, file, "Goober", g.Name,
					"cannot access spec.instructions file %q at %q: %v", g.Spec.Instructions, expected, err)
			case !info.Mode().IsRegular():
				r.add(Error, file, "Goober", g.Name,
					"spec.instructions must name a regular file; %q resolves to %q", g.Spec.Instructions, expected)
			}
		}
	}

	// Workflow state machine integrity.
	for _, indexed := range ix.workflows {
		ix.checkWorkflow(r, indexed.definition, indexed.file, allowPreview)
	}
}

func (ix *index) allowPreviewFeatures(r *Report) bool {
	if len(ix.manifests) != 1 {
		return false
	}
	manifest := ix.manifests[0]
	value, set := manifest.Annotations[wf.PreviewFeaturesAnnotation]
	if !set || value == "false" {
		return false
	}
	if value == "true" {
		return true
	}
	r.add(Error, ix.manifestFile[manifest.Name], "Manifest", manifest.Name,
		"metadata.annotations[%q] must be %q or %q", wf.PreviewFeaturesAnnotation, "true", "false")
	return false
}

// checkGaggleConnections enforces MGV-4's repo-token-ref coherence (#1011):
// every non-empty connectionRef a gaggle uses — on its project repo, any
// additionalRepos entry, or its backlog — must name a Connection declared in
// the Manifest. A dangling reference is reported as an error that names the
// gaggle, the exact field, and the missing connection, so a half-configured
// foreign gaggle fails closed at `validate` time instead of at runtime with an
// opaque credential-resolution failure.
// checkGaggleCICommand enforces MGV-4's CI-command coherence (#1011) over the
// per-gaggle ciCommand (#1009). The schema already rejects an empty command and
// empty elements; the one exec-fatal shape it cannot express is a program
// (argv[0]) that carries whitespace. ciCommand is run directly as argv by the
// local-ci stage (internal/executor exec.Command(name, args...)), never through
// a shell, so a whole-command-as-one-string ["npm run ci"] tries to exec a
// program literally named "npm run ci" and fails to start. Catch it at validate
// time with a message that shows the fix.
func (ix *index) checkGaggleCICommand(r *Report) {
	for name, g := range ix.gaggles {
		if len(g.Spec.CICommand) == 0 {
			continue
		}
		program := g.Spec.CICommand[0]
		if strings.ContainsAny(program, " \t\r\n") {
			r.add(Error, ix.gaggleFile[name], "Gaggle", name,
				"spec.ciCommand program %q contains whitespace; ciCommand is run directly (not through a shell), so the program and each argument must be separate array elements \u2014 e.g. [\"npm\", \"run\", \"ci\"], not [\"npm run ci\"]", program)
		}
	}
}

// checkGaggleBranchNamespace enforces MGV-4's branch-prefix coherence (#1011)
// over the per-gaggle branchNamespace (#965/#1010). The schema pattern already
// enforces the ref-path structure; the gap it cannot express is a value that is
// structurally valid yet produces an INVALID git branch name at runtime, since
// branchNamespace becomes a live run branch "<namespace><workflow>/<run>". git
// rejects a ref with a slash-separated component ending in ".lock" or one that
// contains consecutive dots ".." \u2014 either fails run-branch creation with an
// opaque git error mid-run, exactly the confusing failure MGV-4 pre-empts.
// (Verified against git check-ref-format: a trailing-dot component such as
// "team." IS accepted mid-ref, so it is deliberately not flagged.)
func (ix *index) checkGaggleBranchNamespace(r *Report) {
	for name, g := range ix.gaggles {
		ns := g.Spec.BranchNamespace
		if ns == "" {
			continue
		}
		bad := ""
		if strings.Contains(ns, "..") {
			bad = `contains ".."`
		} else {
			for _, comp := range strings.Split(strings.TrimSuffix(ns, "/"), "/") {
				if strings.HasSuffix(comp, ".lock") {
					bad = fmt.Sprintf("has a component %q ending in \".lock\"", comp)
					break
				}
			}
		}
		if bad != "" {
			r.add(Error, ix.gaggleFile[name], "Gaggle", name,
				"spec.branchNamespace %q %s, which would produce an invalid git run-branch name at runtime", ns, bad)
		}
	}
}

func (ix *index) checkGaggleConnections(r *Report) {
	declared := map[string]bool{}
	for _, m := range ix.manifests {
		for _, c := range m.Spec.Connections {
			if c.Name != "" {
				declared[c.Name] = true
			}
		}
	}
	for name, g := range ix.gaggles {
		file := ix.gaggleFile[name]
		check := func(ref, field string) {
			if ref == "" || declared[ref] {
				return
			}
			r.add(Error, file, "Gaggle", name,
				"%s names connection %q, but no Connection/%s is declared in the Manifest", field, ref, ref)
		}
		check(g.Spec.Project.ConnectionRef, "spec.project.connectionRef")
		check(g.Spec.Backlog.ConnectionRef, "spec.backlog.connectionRef")
		for i, repo := range g.Spec.AdditionalRepos {
			check(repo.ConnectionRef, fmt.Sprintf("spec.additionalRepos[%d].connectionRef", i))
		}
	}
}

func (ix *index) checkWorkflow(r *Report, w apiv1.Workflow, file string, allowPreview bool) {
	if _, ok := ix.gaggles[w.Spec.Gaggle]; !ok {
		r.add(Error, file, "Workflow", w.Name, "spec.gaggle names %q, but no Gaggle/%s definition was found",
			w.Spec.Gaggle, w.Spec.Gaggle)
	}
	r.addFeatureDiagnostics(file, w.Spec.Gaggle, "Workflow", w.Name,
		wf.CheckWorkflowFeatureSupport(wf.Definition{Name: w.Name, Version: 1, Spec: w.Spec}, allowPreview))

	states := map[string]bool{}
	for _, t := range w.Spec.Tasks {
		if states[t.Name] {
			r.add(Error, file, "Workflow", w.Name, "duplicate state name %q", t.Name)
		}
		states[t.Name] = true
	}
	for _, g := range w.Spec.Gates {
		if states[g.Name] {
			r.add(Error, file, "Workflow", w.Name, "duplicate state name %q", g.Name)
		}
		states[g.Name] = true
	}

	if w.Spec.Start != "" && !states[w.Spec.Start] {
		r.add(Error, file, "Workflow", w.Name, "start state %q is not a defined task or gate", w.Spec.Start)
	}

	// Docs-location surface (#1016): a declared docs root must be a usable
	// repo-relative containment root. This is the config-load lexical half —
	// empty / absolute / escaping / whole-repo roots are rejected here, with the
	// same clear message the runtime boundary would carry. A root's existence in
	// the repository is a separate filesystem check the `goobers validate` CLI
	// layers on top (validate.go), since api-level validation has no repo tree.
	for i, dr := range w.Spec.DocsRoots {
		if err := configboundary.ValidateDocsRoot(dr); err != nil {
			r.add(Error, file, "Workflow", w.Name, "spec.docsRoots[%d]: %v", i, err)
		}
	}

	for _, t := range w.Spec.Tasks {
		if t.Type == apiv1.TaskAgentic && t.Goober != "" {
			goober, ok := ix.goobers[t.Goober]
			switch {
			case !ok:
				r.add(Error, file, "Workflow", w.Name, "task %q targets goober %q which is not defined", t.Name, t.Goober)
			case goober.Spec.Gaggle != w.Spec.Gaggle:
				r.add(Error, file, "Workflow", w.Name,
					"task %q targets goober %q in gaggle %q, not workflow gaggle %q",
					t.Name, t.Goober, goober.Spec.Gaggle, w.Spec.Gaggle)
			}
		}
		if t.Next != "" && !wf.IsReservedTarget(t.Next) && !states[t.Next] {
			r.add(Error, file, "Workflow", w.Name, "task %q next state %q is not defined", t.Name, t.Next)
		}
	}

	for _, g := range w.Spec.Gates {
		ix.checkGateEvaluator(r, w, g, file)
		if g.Evaluator == apiv1.EvaluatorAgentic && g.Agentic != nil && g.Agentic.Goober != "" {
			goober, ok := ix.goobers[g.Agentic.Goober]
			switch {
			case !ok:
				r.add(Error, file, "Workflow", w.Name, "gate %q reviewer goober %q is not defined", g.Name, g.Agentic.Goober)
			case goober.Spec.Gaggle != w.Spec.Gaggle:
				r.add(Error, file, "Workflow", w.Name,
					"gate %q reviewer goober %q is in gaggle %q, not workflow gaggle %q",
					g.Name, g.Agentic.Goober, goober.Spec.Gaggle, w.Spec.Gaggle)
			}
		}
		for outcome, next := range g.Branches {
			// Empty means the success terminal (TerminalComplete); "@abort"
			// and "@escalate" are reserved terminal targets — neither is a
			// dangling reference (workflow.IsReservedTarget).
			if next != "" && !wf.IsReservedTarget(next) && !states[next] {
				r.add(Error, file, "Workflow", w.Name, "gate %q branch %q -> %q is not a defined state", g.Name, outcome, next)
			}
		}
	}

	// Delegate the deeper semantic analysis to the workflow compiler so the CLI
	// and the compiler stay in lockstep: reachability + loop-without-exit,
	// schedule-expression validity, and capability/harness admission. These are
	// checks the inline field-by-field pass above deliberately does not duplicate.
	def := wf.Definition{Name: w.Name, Version: 1, Spec: w.Spec}
	for _, msg := range wf.CheckWarnings(def) {
		r.addWarning(WarningCompatibility, file, w.Spec.Gaggle, "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckReachability(def) {
		r.add(Error, file, "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckSchedules(def) {
		r.add(Error, file, "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckGateOutcomes(def) {
		r.add(Error, file, "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckGateParameters(def) {
		r.add(Error, file, "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckTriggerFields(def) {
		r.add(Error, file, "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckWorkflowAdmission(def, ix.gooberSpecs()) {
		r.add(Error, file, "Workflow", w.Name, "%s", msg)
	}
	// Stage output/input contracts (#900). These catch the class of defect
	// that is structurally valid, compiles, and then silently loses data at
	// runtime — a stage promising outputs it has no channel to emit, or
	// reading an upstream output the stage actually preceding it on some
	// branch does not produce. Reported as errors: both are unconditionally
	// broken at runtime, on some path, every time.
	for _, msg := range wf.CheckStageContracts(def) {
		r.add(Error, file, "Workflow", w.Name, "%s", msg)
	}
	// Required-input contracts (#1061). The input-side analog of the above:
	// a deterministic stage that invokes a `goobers` subcommand without
	// wiring an input that subcommand hard-requires. This is what a
	// hand-maintained instance config drifting behind the binary produces —
	// merge-review's apply-verdict losing its selectedHeadSha wiring stalled
	// every election for a full build, and nothing static caught it. Also an
	// error: the stage fails on every run, unconditionally.
	for _, msg := range wf.CheckStageRequiredInputs(def) {
		r.add(Error, file, "Workflow", w.Name, "%s", msg)
	}
	// Only the breaking half is reported here. CheckStageContractWarnings
	// covers the same omission on outputs nothing reads yet, which #881's
	// VER003 "expectedOutputs is declared but not enforced" already warns
	// about for every such stage — emitting both would put two warnings on
	// one missing line. It stays exported for callers that want the strict
	// bar (this repo holds its own shipped workflows to it in
	// internal/workflow's stage-contract test).
}

// gooberSpecs projects the indexed goobers into the name->spec map the compiler's
// capability/harness admission expects.
func (ix *index) gooberSpecs() map[string]apiv1.GooberSpec {
	out := make(map[string]apiv1.GooberSpec, len(ix.goobers))
	for name, g := range ix.goobers {
		out[name] = g.Spec
	}
	return out
}

// checkGateEvaluator enforces GT-016: exactly one evaluator block, matching the
// declared evaluator kind.
func (ix *index) checkGateEvaluator(r *Report, w apiv1.Workflow, g apiv1.Gate, file string) {
	set := 0
	if g.Automated != nil {
		set++
	}
	if g.Agentic != nil {
		set++
	}
	if g.Human != nil {
		set++
	}
	if set != 1 {
		r.add(Error, file, "Workflow", w.Name, "gate %q must have exactly one evaluator block, found %d", g.Name, set)
		return
	}
	mismatch := (g.Evaluator == apiv1.EvaluatorAutomated && g.Automated == nil) ||
		(g.Evaluator == apiv1.EvaluatorAgentic && g.Agentic == nil) ||
		(g.Evaluator == apiv1.EvaluatorHuman && g.Human == nil)
	if mismatch {
		r.add(Error, file, "Workflow", w.Name, "gate %q evaluator=%q but the matching evaluator block is not set", g.Name, g.Evaluator)
	}
}
