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

// Issue is a single validation finding.
type Issue struct {
	Severity Severity `json:"severity"`
	File     string   `json:"file,omitempty"`
	Kind     string   `json:"kind,omitempty"`
	Name     string   `json:"name,omitempty"`
	Message  string   `json:"message"`
}

func (i Issue) String() string {
	loc := i.File
	if i.Kind != "" {
		loc = fmt.Sprintf("%s %s/%s", i.File, i.Kind, i.Name)
	}
	return fmt.Sprintf("%-7s %s: %s", strings.ToUpper(string(i.Severity)), loc, i.Message)
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

func (r *Report) add(sev Severity, file, kind, name, format string, args ...interface{}) {
	r.Issues = append(r.Issues, Issue{
		Severity: sev,
		File:     file,
		Kind:     kind,
		Name:     name,
		Message:  fmt.Sprintf(format, args...),
	})
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

// index holds the typed objects keyed by name for cross-reference checks.
type index struct {
	manifests []apiv1.Manifest
	gaggles   map[string]apiv1.Gaggle
	goobers   map[string]apiv1.Goober
	workflows map[string]apiv1.Workflow
	gooberDir map[string]string // goober name -> source dir (for instruction path checks)

	// manifestDocsSeen counts documents with kind=Manifest regardless of whether
	// they passed schema validation, so we don't double-report "no Manifest" for
	// a manifest that merely failed its schema.
	manifestDocsSeen int
}

func newIndex() *index {
	return &index{
		gaggles:   map[string]apiv1.Gaggle{},
		goobers:   map[string]apiv1.Goober{},
		workflows: map[string]apiv1.Workflow{},
		gooberDir: map[string]string{},
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
	case "Gaggle":
		var g apiv1.Gaggle
		if err := yaml.Unmarshal(doc.json, &g); err != nil {
			r.add(Error, doc.file, doc.kind, doc.name, "decode: %v", err)
			return
		}
		ix.dupCheck(r, doc, "Gaggle", g.Name, func() bool { _, ok := ix.gaggles[g.Name]; return ok })
		ix.gaggles[g.Name] = g
	case "Goober":
		var g apiv1.Goober
		if err := yaml.Unmarshal(doc.json, &g); err != nil {
			r.add(Error, doc.file, doc.kind, doc.name, "decode: %v", err)
			return
		}
		ix.dupCheck(r, doc, "Goober", g.Name, func() bool { _, ok := ix.goobers[g.Name]; return ok })
		ix.goobers[g.Name] = g
		ix.gooberDir[g.Name] = doc.dir
	case "Workflow":
		var w apiv1.Workflow
		if err := yaml.Unmarshal(doc.json, &w); err != nil {
			r.add(Error, doc.file, doc.kind, doc.name, "decode: %v", err)
			return
		}
		ix.dupCheck(r, doc, "Workflow", w.Name, func() bool { _, ok := ix.workflows[w.Name]; return ok })
		ix.workflows[w.Name] = w
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
		r.add(Warning, "", "Manifest", "", "more than one Manifest found (%d); exactly one is expected", len(ix.manifests))
	}

	// Manifest -> gaggle references resolve.
	for _, m := range ix.manifests {
		for _, gname := range m.Spec.Gaggles {
			if _, ok := ix.gaggles[gname]; !ok {
				r.add(Error, "", "Manifest", m.Name, "references gaggle %q which is not defined", gname)
			}
		}
	}

	// Goober -> gaggle / workflow references resolve; instruction file exists.
	for _, g := range ix.goobers {
		if _, ok := ix.gaggles[g.Spec.Gaggle]; !ok {
			r.add(Error, "", "Goober", g.Name, "belongs to gaggle %q which is not defined", g.Spec.Gaggle)
		}
		for _, wf := range g.Spec.Workflows {
			if _, ok := ix.workflows[wf]; !ok {
				r.add(Error, "", "Goober", g.Name, "associated workflow %q is not defined", wf)
			}
		}
		if g.Spec.Instructions != "" {
			p := filepath.Join(ix.gooberDir[g.Name], g.Spec.Instructions)
			if _, err := os.Stat(p); err != nil {
				r.add(Error, "", "Goober", g.Name, "instructions file %q not found (looked in %s)", g.Spec.Instructions, ix.gooberDir[g.Name])
			}
		}
	}

	// Workflow state machine integrity.
	for _, w := range ix.workflows {
		ix.checkWorkflow(r, w)
	}
}

func (ix *index) checkWorkflow(r *Report, w apiv1.Workflow) {
	if _, ok := ix.gaggles[w.Spec.Gaggle]; !ok {
		r.add(Error, "", "Workflow", w.Name, "belongs to gaggle %q which is not defined", w.Spec.Gaggle)
	}

	states := map[string]bool{}
	for _, t := range w.Spec.Tasks {
		if states[t.Name] {
			r.add(Error, "", "Workflow", w.Name, "duplicate state name %q", t.Name)
		}
		states[t.Name] = true
	}
	for _, g := range w.Spec.Gates {
		if states[g.Name] {
			r.add(Error, "", "Workflow", w.Name, "duplicate state name %q", g.Name)
		}
		states[g.Name] = true
	}

	if w.Spec.Start != "" && !states[w.Spec.Start] {
		r.add(Error, "", "Workflow", w.Name, "start state %q is not a defined task or gate", w.Spec.Start)
	}

	for _, t := range w.Spec.Tasks {
		if t.Type == apiv1.TaskAgentic && t.Goober != "" {
			if _, ok := ix.goobers[t.Goober]; !ok {
				r.add(Error, "", "Workflow", w.Name, "task %q targets goober %q which is not defined", t.Name, t.Goober)
			}
		}
		if t.Next != "" && !states[t.Next] {
			r.add(Error, "", "Workflow", w.Name, "task %q next state %q is not defined", t.Name, t.Next)
		}
	}

	for _, g := range w.Spec.Gates {
		ix.checkGateEvaluator(r, w, g)
		if g.Evaluator == apiv1.EvaluatorAgentic && g.Agentic != nil && g.Agentic.Goober != "" {
			if _, ok := ix.goobers[g.Agentic.Goober]; !ok {
				r.add(Error, "", "Workflow", w.Name, "gate %q reviewer goober %q is not defined", g.Name, g.Agentic.Goober)
			}
		}
		for outcome, next := range g.Branches {
			if next != "" && !states[next] {
				r.add(Error, "", "Workflow", w.Name, "gate %q branch %q -> %q is not a defined state", g.Name, outcome, next)
			}
		}
	}

	// Delegate the deeper semantic analysis to the workflow compiler so the CLI
	// and the compiler stay in lockstep: reachability + loop-without-exit,
	// schedule-expression validity, and capability/harness admission. These are
	// checks the inline field-by-field pass above deliberately does not duplicate.
	def := wf.Definition{Name: w.Name, Version: 1, Spec: w.Spec}
	for _, msg := range wf.CheckReachability(def) {
		r.add(Error, "", "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckSchedules(def) {
		r.add(Error, "", "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckAdmission(def, ix.gooberSpecs()) {
		r.add(Error, "", "Workflow", w.Name, "%s", msg)
	}
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
func (ix *index) checkGateEvaluator(r *Report, w apiv1.Workflow, g apiv1.Gate) {
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
		r.add(Error, "", "Workflow", w.Name, "gate %q must have exactly one evaluator block, found %d", g.Name, set)
		return
	}
	mismatch := (g.Evaluator == apiv1.EvaluatorAutomated && g.Automated == nil) ||
		(g.Evaluator == apiv1.EvaluatorAgentic && g.Agentic == nil) ||
		(g.Evaluator == apiv1.EvaluatorHuman && g.Human == nil)
	if mismatch {
		r.add(Error, "", "Workflow", w.Name, "gate %q evaluator=%q but the matching evaluator block is not set", g.Name, g.Evaluator)
	}
}
