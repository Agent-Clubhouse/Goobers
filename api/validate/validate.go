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
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/capability"
	"github.com/goobers/goobers/internal/configboundary"
	"github.com/goobers/goobers/internal/configtree"
	"github.com/goobers/goobers/internal/fieldpredicate"
	"github.com/goobers/goobers/internal/gooberassets"
	"github.com/goobers/goobers/internal/labelpredicate"
	"github.com/goobers/goobers/internal/mcpconfig"
	"github.com/goobers/goobers/internal/runcontrol"
	"github.com/goobers/goobers/internal/supportmatrix"
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
	// WarningSkillPackageCollision identifies a gaggle-scoped skill package
	// shadowing an instance-level package with the same name.
	WarningSkillPackageCollision WarningCode = "SKILL001"
	// WarningMissingDSLVersion identifies a workflow with no dslVersion pin,
	// defaulted to supportmatrix.CurrentDSLVersion during the transition
	// window (DVL-3, #863).
	WarningMissingDSLVersion WarningCode = "DVL001"
	// WarningPreviewDSLVersionOptedIn identifies a workflow pinned to a
	// preview-level dslVersion on an instance that has opted in.
	WarningPreviewDSLVersionOptedIn WarningCode = "DVL010"
	// ErrorPreviewDSLVersionBlocked identifies a workflow pinned to a
	// preview-level dslVersion on an instance that has NOT opted in —
	// closed-by-default (DVL-3, #863).
	ErrorPreviewDSLVersionBlocked WarningCode = "DVL011"
	// WarningDeprecatedDSLVersion identifies a workflow pinned to a
	// deprecated dslVersion — loads, but names its replacement and
	// unsupported-after release.
	WarningDeprecatedDSLVersion WarningCode = "DVL020"
	// ErrorUnsupportedDSLVersion identifies a workflow pinned to a dslVersion
	// this binary either does not recognize or has marked unsupported — fails
	// load like a schema violation.
	ErrorUnsupportedDSLVersion WarningCode = "DVL030"
	// WarningSiblingLabelOverlap identifies a gaggle whose declared sibling
	// (MIRC-2, #1901) targets the same repo and has an effective
	// requireLabels scope that is not disjoint from this gaggle's own, or this
	// gaggle has no effective requireLabels partition at all. Non-fatal: it
	// does not change any two instances' actual runtime behavior by itself,
	// it only surfaces the misconfiguration risk before it produces a live
	// claim collision.
	WarningSiblingLabelOverlap WarningCode = "SIB001"
	// WarningMissingSkillPackage identifies a declared goober skill whose
	// package directory is absent.
	WarningMissingSkillPackage WarningCode = "SKILL002"
	// WarningUnclaimedRunnerCapability identifies a gaggle/stage
	// requiredCapabilities token that instance.yaml's runner.capabilities
	// does not claim (RRQ-1/#1101). Schedule-time matching is an exact
	// string set-membership check (internal/runnercap), so an unclaimed
	// token means the scheduler refuses placement of every run of that
	// gaggle and `goobers up` fails closed at startup
	// (instance.CheckCapabilityRequirements) — a structural no-run state
	// the config validator can see statically because it reads both files
	// in the same pass (2026-08-08 cold-start audit, dotnet #7 / swift
	// probes).
	WarningUnclaimedRunnerCapability WarningCode = "CAP003"
	// WarningMaxOpenPRsUnenforceable identifies a workflow whose maxOpenPRs
	// readiness cap cannot obtain a GitHub open-PR count for its gaggle's
	// project repository.
	WarningMaxOpenPRsUnenforceable WarningCode = "PRCAP001"
	// WarningGateCompletionHidesFailure identifies an automated gate branch
	// that is keyed on a failure-implying outcome (status-equals'
	// default/success "fail", failure-class "fail"/"infra") and routes to
	// workflow completion (""), while a stage feeding that gate does not set
	// continueOnError. The branch IS taken — a failed stage whose `next`
	// names a gate always delivers its honest failed status to the gate
	// (internal/runner taskOutcome) — but the run then terminates failed,
	// not completed: the runner refuses to complete a run whose final stage
	// failure was neither tolerated (continueOnError) nor affirmatively
	// cleared by a pass/human verdict (#849's unresolved-failure rule). The
	// declared completion is therefore unreachable dead config (2026-08-08
	// cold-start audit, swift #3's verified shape).
	WarningGateCompletionHidesFailure WarningCode = "WF018"
)

const (
	errorInvalidGooberAssets      WarningCode = "ASSET001"
	errorInvalidYAML              WarningCode = "YAML001"
	errorMissingTypeMeta          WarningCode = "SCHEMA001"
	errorUnknownKind              WarningCode = "SCHEMA002"
	errorSchemaViolation          WarningCode = "SCHEMA003"
	errorTypedDecode              WarningCode = "SCHEMA004"
	errorDuplicateDefinition      WarningCode = "CFG001"
	errorMissingManifest          WarningCode = "CFG002"
	errorMultipleManifests        WarningCode = "CFG003"
	errorPreviewAnnotation        WarningCode = "CFG004"
	errorCICommand                WarningCode = "CFG005"
	errorBranchNamespace          WarningCode = "CFG006"
	errorGaggleCheckoutSparse     WarningCode = "CFG007"
	errorManifestGaggleReference  WarningCode = "REF001"
	errorGooberGaggleReference    WarningCode = "REF002"
	errorGooberWorkflowReference  WarningCode = "REF003"
	errorConnectionReference      WarningCode = "REF004"
	errorAdditionalRepoProject    WarningCode = "REF005"
	errorAdditionalRepoDuplicate  WarningCode = "REF006"
	errorWorkflowGaggleReference  WarningCode = "REF007"
	errorTaskGooberReference      WarningCode = "REF008"
	errorTaskGooberGaggle         WarningCode = "REF009"
	errorGateGooberReference      WarningCode = "REF010"
	errorGateGooberGaggle         WarningCode = "REF011"
	errorRunnerCapability         WarningCode = "CAP001"
	errorUnknownCapability        WarningCode = "CAP002"
	errorInstructionsMissing      WarningCode = "GBO001"
	errorInstructionsAccess       WarningCode = "GBO002"
	errorInstructionsNotRegular   WarningCode = "GBO003"
	errorMCPConfig                WarningCode = "MCP001"
	errorDuplicateState           WarningCode = "WF001"
	errorStartState               WarningCode = "WF002"
	errorTaskNextState            WarningCode = "WF003"
	errorGateBranch               WarningCode = "WF004"
	errorReachability             WarningCode = "WF005"
	errorSchedule                 WarningCode = "WF006"
	errorGateOutcome              WarningCode = "WF007"
	errorGateParameter            WarningCode = "WF008"
	errorTriggerField             WarningCode = "WF009"
	errorWorkflowAdmission        WarningCode = "WF010"
	errorStageContract            WarningCode = "WF011"
	errorStageRequiredInput       WarningCode = "WF012"
	errorStageTimeout             WarningCode = "WF013"
	errorGateEvaluatorCardinality WarningCode = "WF014"
	errorGateEvaluatorMismatch    WarningCode = "WF015"
	errorRunControls              WarningCode = "WF016"
	errorPathSimulation           WarningCode = "WF017"
	errorCapabilityRuntimeSupport WarningCode = "WF018"
	errorDocsRoot                 WarningCode = "DOCS001"
	errorUnsupportedFeature       WarningCode = "VER005"
	errorLabelPredicateGaggle     WarningCode = "LBL001"
	errorLabelPredicateTrigger    WarningCode = "LBL002"
	errorLabelPredicateTaskBlank  WarningCode = "LBL003"
	errorLabelPredicateTask       WarningCode = "LBL004"
	errorFieldPredicateGaggle     WarningCode = "FLD001"
	errorFieldPredicateTrigger    WarningCode = "FLD002"
	errorFieldPredicateTask       WarningCode = "FLD003"
	errorFieldOrderTask           WarningCode = "FLD004"
	errorTutorScopeTarget         WarningCode = "TUT001"
	warningPRLifecycleBaseDrift   WarningCode = "PRB001"
	errorContextFromDuplicate     WarningCode = "CTX001"
)

const acknowledgeManualOnlyAnnotation = "goobers.dev/acknowledge-manual-only"

// Issue is a single validation finding.
type Issue struct {
	Code     WarningCode `json:"code,omitempty"`
	Severity Severity    `json:"severity"`
	File     string      `json:"file,omitempty"`
	Line     int         `json:"-"`
	Col      int         `json:"-"`
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
	return fmt.Sprintf("%-7s%s %s: %s%s", strings.ToUpper(string(i.Severity)), code, i.Scope(), i.Message, i.position())
}

// invalidYAMLMessagePrefix marks a message as errorInvalidYAML's own —
// checked by content rather than Code since cliIssue() already strips Code
// off a plain Error by the time String() runs via CLIString().
const invalidYAMLMessagePrefix = "invalid YAML: "

// position renders a resolved source line (and column, when known) as a
// trailing suffix, e.g. " (line 15, col 3)" — appended after the message so
// the established "SEVERITY[ CODE] Scope: Message" prefix an existing
// consumer may match against never changes (#2025). Empty when Line is 0
// (unresolved — e.g. a required-but-entirely-absent property has no node to
// point at) or for an invalid-YAML message, which already embeds its own
// "yaml: line N: ..." position from the underlying parser — a second,
// differently-numbered suffix there would be redundant noise, not new
// information.
func (i Issue) position() string {
	if strings.HasPrefix(i.Message, invalidYAMLMessagePrefix) {
		return ""
	}
	switch {
	case i.Line > 0 && i.Col > 0:
		return fmt.Sprintf(" (line %d, col %d)", i.Line, i.Col)
	case i.Line > 0:
		return fmt.Sprintf(" (line %d)", i.Line)
	default:
		return ""
	}
}

// CLIString preserves the validator's established text representation while
// structured consumers use the richer warning provenance.
func (i Issue) CLIString() string {
	return i.cliIssue().String()
}

func (i Issue) cliIssue() Issue {
	if i.Severity == Error && i.Code != WarningPreviewFeature && i.Code != ErrorRemovedFeature {
		i.Code = ""
	}
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
	if w.Code == "" {
		return fmt.Sprintf("%s %s: %s", strings.ToUpper(string(w.Severity)), w.Scope, w.Explanation)
	}
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

func (r *Report) add(code WarningCode, sev Severity, file, kind, name, format string, args ...interface{}) {
	r.addCoded(code, sev, file, kind, name, format, args...)
}

func (r *Report) addCoded(code WarningCode, sev Severity, file, kind, name, format string, args ...interface{}) {
	r.addLocated(code, sev, file, 0, 0, kind, name, format, args...)
}

func (r *Report) addLocated(
	code WarningCode,
	sev Severity,
	file string,
	line, col int,
	kind, name, format string,
	args ...interface{},
) {
	r.Issues = append(r.Issues, Issue{
		Code:     code,
		Severity: sev,
		File:     file,
		Line:     line,
		Col:      col,
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
		default:
			code = errorUnsupportedFeature
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

var (
	docSep          = regexp.MustCompile(`(?m)^---\s*$`)
	yamlLinePattern = regexp.MustCompile(`\bline ([0-9]+)\b`)
)

type yamlDocument struct {
	content    string
	lineOffset int
}

func splitYAMLDocuments(raw string) []yamlDocument {
	separators := docSep.FindAllStringIndex(raw, -1)
	documents := make([]yamlDocument, 0, len(separators)+1)
	start, lineOffset := 0, 0
	for _, separator := range separators {
		documents = append(documents, yamlDocument{
			content:    raw[start:separator[0]],
			lineOffset: lineOffset,
		})
		lineOffset += strings.Count(raw[start:separator[1]], "\n")
		start = separator[1]
	}
	return append(documents, yamlDocument{
		content:    raw[start:],
		lineOffset: lineOffset,
	})
}

func yamlErrorLine(message string, lineOffset int) int {
	match := yamlLinePattern.FindStringSubmatch(message)
	if len(match) != 2 {
		return 0
	}
	line, err := strconv.Atoi(match[1])
	if err != nil {
		return 0
	}
	return line + lineOffset
}

// typeMeta is the minimal shape needed to dispatch a document to its schema.
type typeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	DSLVersion string `json:"dslVersion"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
}

// loadedDoc is one parsed YAML document plus provenance.
type loadedDoc struct {
	file       string
	dir        string
	kind       string
	name       string
	dslVersion string
	json       []byte
	// node is the document's own YAML source, reparsed with gopkg.in/yaml.v3
	// (which preserves node positions, unlike the sigs.k8s.io/yaml round-trip
	// used to build json above). It resolves a schema violation's source line
	// and column (#2025); nil only if this content somehow parses via
	// yaml.YAMLToJSON but not yaml.v3 (not expected in practice). node's own
	// Line/Column are relative to this document's own content (line 1 = the
	// document's first line) — lineOffset converts that to the file's actual
	// line, the same convention yamlErrorLine already uses for syntax errors.
	node       *yamlv3.Node
	lineOffset int
}

// ValidateDir validates every YAML object under root: schema-checks each, then
// applies cross-object reference rules. The returned Report is always non-nil.
func (v *Validator) ValidateDir(root string) (*Report, error) {
	r := &Report{}
	var docs []loadedDoc
	parseFailureCount := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.IsDir() && configtree.IsGaggleSkillsDir(root, path) {
			return filepath.SkipDir
		}
		if gooberassets.IsSourceDir(path) {
			if assetErr := gooberassets.Validate(path); assetErr != nil {
				rel, _ := filepath.Rel(root, path)
				r.add(errorInvalidGooberAssets, Error, filepath.ToSlash(rel), "", "", "invalid goober assets: %v", assetErr)
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
		for _, document := range splitYAMLDocuments(string(raw)) {
			if strings.TrimSpace(document.content) == "" {
				continue
			}
			jb, err := yaml.YAMLToJSON([]byte(document.content))
			if err != nil {
				parseFailureCount++
				r.addLocated(errorInvalidYAML, Error, rel,
					yamlErrorLine(err.Error(), document.lineOffset), 1,
					"", "", invalidYAMLMessagePrefix+"%s", err)
				continue
			}
			var tm typeMeta
			if err := json.Unmarshal(jb, &tm); err != nil || tm.Kind == "" {
				r.add(errorMissingTypeMeta, Error, rel, "", "", "document is missing apiVersion/kind")
				continue
			}
			docs = append(docs, loadedDoc{
				file: rel, dir: filepath.Dir(path), kind: tm.Kind, name: tm.Metadata.Name,
				dslVersion: tm.DSLVersion, json: jb,
				node: parseYAMLNode(document.content), lineOffset: document.lineOffset,
			})
		}
		return nil
	})
	if err != nil {
		return r, fmt.Errorf("walk %s: %w", root, err)
	}

	idx := newIndex()
	idx.parseFailureCount = parseFailureCount
	for _, doc := range docs {
		r.Objects++
		if doc.kind == "Manifest" {
			idx.manifestDocsSeen++
		}
		schemaFile, ok := schemas.Kind[doc.kind]
		if !ok {
			r.add(errorUnknownKind, Error, doc.file, doc.kind, doc.name, "unknown kind %q", doc.kind)
			continue
		}
		if err := v.ValidateJSON(schemaFile, doc.json); err != nil {
			schema, _ := v.schema(schemaFile)
			for _, finding := range schemaFindings(err, schema, doc.node) {
				if finding.line > 0 {
					r.addLocated(errorSchemaViolation, Error, doc.file,
						finding.line+doc.lineOffset, finding.col,
						doc.kind, doc.name, "%s", finding.message)
					continue
				}
				r.add(errorSchemaViolation, Error, doc.file, doc.kind, doc.name, "%s", finding.message)
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

	idx.crossCheck(r, root)
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

	// parseFailureCount counts documents in this run that failed to parse as
	// YAML at all (errorInvalidYAML). A cross-reference "no X/Y definition was
	// found" error may just be that document's own missing definition rather
	// than a second, independent problem — but only when there is exactly one
	// parse failure and exactly one reference gap in the whole run: with
	// multiple of either, correlating a specific gap to a specific failure
	// isn't something we can actually know (we never learn the failed
	// document's own kind/name), and guessing would mislabel a genuinely
	// independent, unrelated bug as a probable side effect of the parse
	// failure. referenceNotFound buffers into pendingReferenceIssues so this
	// can be decided once, after every reference check in the run has run —
	// not fired incrementally as each one is discovered (#2025, QA-2 finding
	// 1).
	parseFailureCount      int
	pendingReferenceIssues []pendingReferenceIssue
}

// pendingReferenceIssue is a "no X/Y definition was found"-style
// cross-reference error, held until crossCheck finishes so its subordination
// note (see parseFailureCount) can be applied — or not — based on the full
// run's outcome, not just what's known when the gap is first discovered.
type pendingReferenceIssue struct {
	code             WarningCode
	file, kind, name string
	message          string
}

// referenceNotFound records a cross-reference error for a name this config
// doesn't define anywhere. See parseFailureCount and flushReferenceIssues.
func (ix *index) referenceNotFound(r *Report, code WarningCode, file, kind, name, format string, args ...interface{}) {
	ix.pendingReferenceIssues = append(ix.pendingReferenceIssues, pendingReferenceIssue{
		code: code, file: file, kind: kind, name: name,
		message: fmt.Sprintf(format, args...),
	})
}

// flushReferenceIssues adds every buffered referenceNotFound call to r,
// appending the parse-failure subordination note only when the run had
// exactly one parse failure and exactly one reference gap — the one
// situation where attributing the gap to the failure is actually
// well-founded, not a guess (#2025, QA-2 finding 1). Must run once, after
// every check in crossCheck that can call referenceNotFound has run.
func (ix *index) flushReferenceIssues(r *Report) {
	subordinate := ix.parseFailureCount == 1 && len(ix.pendingReferenceIssues) == 1
	for _, issue := range ix.pendingReferenceIssues {
		message := issue.message
		if subordinate {
			message += " (a document elsewhere failed to parse as YAML — see the invalid-YAML error above; this may be its own missing definition rather than a separate problem)"
		}
		r.add(issue.code, Error, issue.file, issue.kind, issue.name, "%s", message)
	}
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
			r.add(errorTypedDecode, Error, doc.file, doc.kind, doc.name, "decode: %v", err)
			return
		}
		ix.manifests = append(ix.manifests, m)
		ix.manifestFile[m.Name] = doc.file
	case "Gaggle":
		var g apiv1.Gaggle
		if err := yaml.Unmarshal(doc.json, &g); err != nil {
			r.add(errorTypedDecode, Error, doc.file, doc.kind, doc.name, "decode: %v", err)
			return
		}
		ix.dupCheck(r, doc, "Gaggle", g.Name, func() bool { _, ok := ix.gaggles[g.Name]; return ok })
		ix.gaggles[g.Name] = g
		ix.gaggleFile[g.Name] = doc.file
	case "Goober":
		var g apiv1.Goober
		if err := yaml.Unmarshal(doc.json, &g); err != nil {
			r.add(errorTypedDecode, Error, doc.file, doc.kind, doc.name, "decode: %v", err)
			return
		}
		ix.dupCheck(r, doc, "Goober", g.Name, func() bool { _, ok := ix.goobers[g.Name]; return ok })
		ix.goobers[g.Name] = g
		ix.gooberFile[g.Name] = doc.file
		ix.gooberDir[g.Name] = doc.dir
	case "Workflow":
		var w apiv1.Workflow
		if err := yaml.Unmarshal(doc.json, &w); err != nil {
			r.add(errorTypedDecode, Error, doc.file, doc.kind, doc.name, "decode: %v", err)
			return
		}
		w.DSLVersion = doc.dslVersion
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
		r.add(errorDuplicateDefinition, Error, doc.file, kind, name, "duplicate %s name %q", kind, name)
	}
}

// crossCheck applies the spec's reference rules across all loaded objects.
func (ix *index) crossCheck(r *Report, configRoot string) {
	if len(ix.manifests) == 0 && ix.manifestDocsSeen == 0 {
		r.add(errorMissingManifest, Error, "", "Manifest", "", "no Manifest object found in config directory")
	}
	if len(ix.manifests) > 1 {
		// Error, not Warning (#243): internal/instance/configdir.go and
		// internal/configsync/loader.go both reject a config directory with
		// more than one Manifest outright — a validate-only consumer must
		// not report success-with-warning for a config the daemon actually
		// refuses to load.
		r.add(errorMultipleManifests, Error, "", "Manifest", "", "more than one Manifest found (%d); exactly one is expected", len(ix.manifests))
	}
	allowPreview := ix.allowPreviewFeatures(r)

	// Manifest -> gaggle references resolve.
	for _, m := range ix.manifests {
		for _, gname := range m.Spec.Gaggles {
			if _, ok := ix.gaggles[gname]; !ok {
				ix.referenceNotFound(r, errorManifestGaggleReference, ix.manifestFile[m.Name], "Manifest", m.Name,
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
	// Read-only reference-repo coherence (MGV-10, #1285): an AdditionalRepos
	// entry must not also be the gaggle's read-write Project.
	ix.checkGaggleAdditionalRepos(r)
	// Gaggle CI-command coherence (MGV-4) over #1009's ciCommand surface.
	ix.checkGaggleCICommand(r)
	// Gaggle branch-prefix coherence (MGV-4) over #965/#1010's branchNamespace surface.
	ix.checkGaggleBranchNamespace(r)
	// Sibling-scope overlap warning (MIRC-2, #1901).
	ix.checkGaggleSiblingLabelOverlap(r)
	ix.checkGaggleRunControls(r)
	// Accepted-but-inert checkout declarations (#649) surface a VER003 notice.
	ix.checkGaggleCheckout(r)
	ix.checkLabelPredicates(r)
	ix.checkContextFromUniqueness(r)
	ix.checkFieldSelections(r)
	for name, g := range ix.gaggles {
		for _, def := range ix.featureDefinitionsForGaggle(name) {
			r.addFeatureDiagnostics(ix.gaggleFile[name], name, "Gaggle", name,
				wf.CheckGaggleFeatureSupport(def, g.Spec, allowPreview))
		}
	}
	// Goober -> gaggle / workflow references resolve; instruction file exists.
	for _, g := range ix.goobers {
		file := ix.gooberFile[g.Name]
		for _, def := range ix.featureDefinitionsForGoober(g.Spec) {
			r.addFeatureDiagnostics(file, g.Spec.Gaggle, "Goober", g.Name,
				wf.CheckGooberFeatureSupport(def, g.Spec, allowPreview))
		}
		if _, ok := ix.gaggles[g.Spec.Gaggle]; !ok {
			ix.referenceNotFound(r, errorGooberGaggleReference, file, "Goober", g.Name, "spec.gaggle names %q, but no Gaggle/%s definition was found",
				g.Spec.Gaggle, g.Spec.Gaggle)
		}
		for _, wf := range g.Spec.Workflows {
			identity := workflowIdentity{gaggle: g.Spec.Gaggle, name: wf}
			if _, ok := ix.workflows[identity]; !ok {
				ix.referenceNotFound(r, errorGooberWorkflowReference, file, "Goober", g.Name,
					"spec.workflows references %q, but no Workflow/%s is defined in gaggle %q",
					wf, wf, g.Spec.Gaggle)
			}
		}
		for _, value := range g.Spec.Capabilities {
			if capability.Known(value) {
				if !capability.StageDeclarable(value) {
					r.add(
						errorRunnerCapability,
						Error,
						file,
						"Goober",
						g.Name,
						"spec.capabilities contains runner-only capability %q",
						value,
					)
				}
				continue
			}
			message := fmt.Sprintf("spec.capabilities contains unknown capability %q", value)
			if suggestion, ok := capability.Suggest(value); ok {
				message += fmt.Sprintf("; did you mean %q?", suggestion)
			}
			r.add(errorUnknownCapability, Error, file, "Goober", g.Name, "%s", message)
		}
		if err := mcpconfig.ValidateForHarness(g.Spec.Harness, g.Spec.MCPServers, g.Spec.Capabilities, g.Spec.Tools); err != nil {
			r.add(errorMCPConfig, Error, file, "Goober", g.Name, "spec.%v", err)
		}
		if g.Spec.Instructions != "" {
			p := filepath.Join(ix.gooberDir[g.Name], g.Spec.Instructions)
			info, err := os.Stat(p)
			expected := filepath.ToSlash(filepath.Join(filepath.Dir(file), g.Spec.Instructions))
			switch {
			case errors.Is(err, fs.ErrNotExist):
				r.add(errorInstructionsMissing, Error, file, "Goober", g.Name,
					"spec.instructions file %q was not found; expected it at %q", g.Spec.Instructions, expected)
			case err != nil:
				r.add(errorInstructionsAccess, Error, file, "Goober", g.Name,
					"cannot access spec.instructions file %q at %q: %v", g.Spec.Instructions, expected, err)
			case !info.Mode().IsRegular():
				r.add(errorInstructionsNotRegular, Error, file, "Goober", g.Name,
					"spec.instructions must name a regular file; %q resolves to %q", g.Spec.Instructions, expected)
			}
		}
	}

	// Workflow state machine integrity.
	for _, indexed := range ix.workflows {
		ix.checkWorkflow(r, indexed.definition, indexed.file, allowPreview)
		checkWorkflowDSLVersion(r, indexed.definition, indexed.file, allowPreview)
	}

	// Every referenceNotFound call in this pass (including from checkWorkflow
	// above) was buffered, not yet added to r — flush now that the run's full
	// outcome (how many parse failures, how many reference gaps) is known.
	ix.flushReferenceIssues(r)
	ix.checkMissingSkillPackages(r, configRoot)
}

func (ix *index) featureDefinitionsForGaggle(gaggle string) []wf.Definition {
	byVersion := map[string]wf.Definition{}
	for identity, indexed := range ix.workflows {
		if identity.gaggle != gaggle {
			continue
		}
		definition := indexed.definition
		byVersion[definition.DSLVersion] = wf.Definition{
			Name: definition.Name, DSLVersion: definition.DSLVersion, Spec: definition.Spec,
		}
	}
	if len(byVersion) == 0 {
		return []wf.Definition{{}}
	}
	versions := make([]string, 0, len(byVersion))
	for version := range byVersion {
		versions = append(versions, version)
	}
	sort.Strings(versions)
	definitions := make([]wf.Definition, 0, len(versions))
	for _, version := range versions {
		definitions = append(definitions, byVersion[version])
	}
	return definitions
}

func (ix *index) featureDefinitionsForGoober(spec apiv1.GooberSpec) []wf.Definition {
	byVersion := map[string]wf.Definition{}
	for _, name := range spec.Workflows {
		indexed, ok := ix.workflows[workflowIdentity{gaggle: spec.Gaggle, name: name}]
		if !ok {
			continue
		}
		definition := indexed.definition
		byVersion[definition.DSLVersion] = wf.Definition{
			Name: definition.Name, DSLVersion: definition.DSLVersion, Spec: definition.Spec,
		}
	}
	if len(byVersion) == 0 {
		return []wf.Definition{{}}
	}
	versions := make([]string, 0, len(byVersion))
	for version := range byVersion {
		versions = append(versions, version)
	}
	sort.Strings(versions)
	definitions := make([]wf.Definition, 0, len(versions))
	for _, version := range versions {
		definitions = append(definitions, byVersion[version])
	}
	return definitions
}

func declaredSkillPackageDirs(configRoot, gaggle, skill string) (scoped, shared string, ok bool) {
	if skill == "" || skill == "." || skill == ".." || strings.ContainsAny(skill, `/\`) || filepath.VolumeName(skill) != "" {
		return "", "", false
	}
	configRoot = filepath.Clean(configRoot)
	return filepath.Join(configRoot, "gaggles", gaggle, "skills", skill),
		filepath.Join(filepath.Dir(configRoot), "skills", skill), true
}

func (ix *index) checkMissingSkillPackages(r *Report, configRoot string) {
	for _, g := range ix.goobers {
		for _, skill := range g.Spec.Skills {
			scoped, shared, ok := declaredSkillPackageDirs(configRoot, g.Spec.Gaggle, skill)
			if !ok {
				r.add(WarningMissingSkillPackage, Warning, ix.gooberFile[g.Name], "Goober", g.Name,
					"spec.skills declares %q, but the skill name cannot resolve to a package directory under %q",
					skill, "skills")
				continue
			}
			scopedInfo, scopedErr := os.Stat(scoped)
			sharedInfo, sharedErr := os.Stat(shared)
			scopedMissing := errors.Is(scopedErr, fs.ErrNotExist) || (scopedErr == nil && !scopedInfo.IsDir())
			sharedMissing := errors.Is(sharedErr, fs.ErrNotExist) || (sharedErr == nil && !sharedInfo.IsDir())
			if scopedMissing && sharedMissing {
				r.add(WarningMissingSkillPackage, Warning, ix.gooberFile[g.Name], "Goober", g.Name,
					"spec.skills declares %q, but no skill package directory was found at %q or %q",
					skill,
					filepath.ToSlash(filepath.Join("gaggles", g.Spec.Gaggle, "skills", skill)),
					filepath.ToSlash(filepath.Join("skills", skill)))
			}
		}
	}
}

// dslSupportMatrix resolves the current binary's DSL version support matrix.
// A package var (rather than a direct supportmatrix.GetDSL() call) so tests
// can exercise the preview/deprecated/unsupported diagnostics against a
// synthetic matrix without mutating the live, compiled-in registry.
var dslSupportMatrix = supportmatrix.GetDSL

// checkWorkflowDSLVersion enforces the DSL version support lifecycle (DVL-3,
// #863) at config-load time — the direct fix for the drift incident in
// docs/design/dsl-version-lifecycle.md §1: a workflow's dslVersion pin is
// checked against this binary's declared supportmatrix.SupportMatrix, so an
// unsupported or blocked-preview pin fails here, with a clear diagnostic,
// instead of surfacing later as an opaque interpreterForVersion compile
// error. The default this applies to a missing pin (supportmatrix.
// CurrentDSLVersion) is deliberately the exact same default
// internal/workflow.Compile's own interpreterForVersion falls back to, so
// this check can never disagree with what actually compiles and runs.
//
// This is the sole enforcement point for the lifecycle: internal/configsync's
// daemon load path and instance.LoadConfigDir's offline CLI path both route
// through this same Validator.ValidateDir → crossCheck call, so neither can
// drift from the other.
func checkWorkflowDSLVersion(r *Report, w apiv1.Workflow, file string, allowPreview bool) {
	version := w.DSLVersion
	if version == "" {
		version = supportmatrix.CurrentDSLVersion
		r.addWarning(WarningMissingDSLVersion, file, w.Spec.Gaggle, "Workflow", w.Name,
			"spec has no dslVersion pin; defaulting to %q during the transition window — pin an explicit dslVersion before this becomes a hard error", version)
	}

	support, ok := dslSupportMatrix().Lookup(version)
	if !ok {
		r.addCoded(ErrorUnsupportedDSLVersion, Error, file, "Workflow", w.Name,
			"dslVersion %q is not a version this binary recognizes; known versions: %s",
			version, strings.Join(knownDSLVersions(), ", "))
		return
	}

	switch support.Level {
	case supportmatrix.LevelPreview:
		if !allowPreview {
			r.addCoded(ErrorPreviewDSLVersionBlocked, Error, file, "Workflow", w.Name,
				"dslVersion %q is preview and this instance has not opted in; set metadata.annotations[%q]=%q on the Manifest to allow it",
				version, wf.PreviewFeaturesAnnotation, "true")
			return
		}
		r.addWarning(WarningPreviewDSLVersionOptedIn, file, w.Spec.Gaggle, "Workflow", w.Name,
			"dslVersion %q is preview; this instance has opted in via metadata.annotations[%q]", version, wf.PreviewFeaturesAnnotation)
	case supportmatrix.LevelDeprecated:
		r.addWarning(WarningDeprecatedDSLVersion, file, w.Spec.Gaggle, "Workflow", w.Name,
			"dslVersion %q is deprecated (replacement %q, unsupported after %s); migrate with `goobers fix --to %s`",
			version, support.Replacement, support.UnsupportedAfter, support.Replacement)
	case supportmatrix.LevelUnsupported:
		r.addCoded(ErrorUnsupportedDSLVersion, Error, file, "Workflow", w.Name,
			"dslVersion %q is unsupported by this binary (replacement %q); migrate with `goobers fix --to %s` before upgrading",
			version, support.Replacement, support.Replacement)
	case supportmatrix.LevelSupported:
		// Nothing to report — the common case.
	}
}

func knownDSLVersions() []string {
	versions := dslSupportMatrix().Versions()
	names := make([]string, len(versions))
	for i, v := range versions {
		names[i] = v.Version
	}
	return names
}

func (ix *index) checkLabelPredicates(r *Report) {
	for name, gaggle := range ix.gaggles {
		expression := gaggle.Spec.Backlog.LabelPredicate
		if expression == "" {
			continue
		}
		if _, err := labelpredicate.Compile(expression, gaggle.Spec.Backlog.Labels, nil); err != nil {
			r.add(errorLabelPredicateGaggle, Error, ix.gaggleFile[name], "Gaggle", name,
				"spec.backlog.labelPredicate is invalid: %v", err)
		}
	}
	for _, indexed := range ix.workflows {
		workflow := indexed.definition
		for i, trigger := range workflow.Spec.Triggers {
			if trigger.LabelPredicate == "" {
				continue
			}
			required := make([]string, 0, len(trigger.Selector))
			for label := range trigger.Selector {
				required = append(required, label)
			}
			if _, err := labelpredicate.Compile(trigger.LabelPredicate, required, nil); err != nil {
				r.add(errorLabelPredicateTrigger, Error, indexed.file, "Workflow", workflow.Name,
					"spec.triggers[%d].labelPredicate is invalid: %v", i, err)
			}
		}
		for i, task := range workflow.Spec.Tasks {
			if !isBacklogQueryTask(task) {
				continue
			}
			expression, ok := task.Inputs["labelPredicate"]
			if !ok {
				continue
			}
			if strings.TrimSpace(expression) == "" {
				r.add(errorLabelPredicateTaskBlank, Error, indexed.file, "Workflow", workflow.Name,
					"spec.tasks[%d].inputs.labelPredicate is invalid: CEL expression must not be blank", i)
				continue
			}
			if _, err := labelpredicate.Compile(
				expression,
				splitLabelInput(task.Inputs["requireLabels"]),
				splitLabelInput(task.Inputs["excludeLabels"]),
			); err != nil {
				r.add(errorLabelPredicateTask, Error, indexed.file, "Workflow", workflow.Name,
					"spec.tasks[%d].inputs.labelPredicate is invalid: %v", i, err)
			}
		}
	}
}

// checkContextFromUniqueness rejects duplicate entries in a task's contextFrom.
//
// This lived on the Go type as +kubebuilder:validation:UniqueItems=true until
// that marker was found to make the generated CRD un-installable: Kubernetes
// forbids uniqueItems in a structural schema because the runtime complexity is
// quadratic. The constraint is still worth enforcing, just not there.
func (ix *index) checkContextFromUniqueness(r *Report) {
	for _, indexed := range ix.workflows {
		workflow := indexed.definition
		for i, task := range workflow.Spec.Tasks {
			seen := make(map[string]struct{}, len(task.ContextFrom))
			for _, source := range task.ContextFrom {
				if _, duplicate := seen[source]; duplicate {
					r.add(errorContextFromDuplicate, Error, indexed.file, "Workflow", workflow.Name,
						"spec.tasks[%d].contextFrom lists %q more than once", i, source)
					continue
				}
				seen[source] = struct{}{}
			}
		}
	}
}

func (ix *index) checkFieldSelections(r *Report) {
	for name, gaggle := range ix.gaggles {
		expression := gaggle.Spec.Backlog.FieldPredicate
		if expression == "" {
			continue
		}
		if _, err := fieldpredicate.Compile(expression); err != nil {
			r.add(errorFieldPredicateGaggle, Error, ix.gaggleFile[name], "Gaggle", name,
				"spec.backlog.fieldPredicate is invalid: %v", err)
		}
	}
	for _, indexed := range ix.workflows {
		workflow := indexed.definition
		for i, trigger := range workflow.Spec.Triggers {
			if trigger.FieldPredicate == "" {
				continue
			}
			if _, err := fieldpredicate.Compile(trigger.FieldPredicate); err != nil {
				r.add(errorFieldPredicateTrigger, Error, indexed.file, "Workflow", workflow.Name,
					"spec.triggers[%d].fieldPredicate is invalid: %v", i, err)
			}
		}
		for i, task := range workflow.Spec.Tasks {
			if !isBacklogQueryTask(task) {
				continue
			}
			if expression, ok := task.Inputs["fieldPredicate"]; ok {
				if strings.TrimSpace(expression) == "" {
					r.add(errorFieldPredicateTask, Error, indexed.file, "Workflow", workflow.Name,
						"spec.tasks[%d].inputs.fieldPredicate is invalid: CEL expression must not be blank", i)
				} else if _, err := fieldpredicate.Compile(expression); err != nil {
					r.add(errorFieldPredicateTask, Error, indexed.file, "Workflow", workflow.Name,
						"spec.tasks[%d].inputs.fieldPredicate is invalid: %v", i, err)
				}
			}
			if expression, ok := task.Inputs["fieldOrder"]; ok {
				if strings.TrimSpace(expression) == "" {
					r.add(errorFieldOrderTask, Error, indexed.file, "Workflow", workflow.Name,
						"spec.tasks[%d].inputs.fieldOrder is invalid: field order must not be blank", i)
				} else if _, err := fieldpredicate.ParseOrder(expression); err != nil {
					r.add(errorFieldOrderTask, Error, indexed.file, "Workflow", workflow.Name,
						"spec.tasks[%d].inputs.fieldOrder is invalid: %v", i, err)
				}
			}
		}
	}
}

func isBacklogQueryTask(task apiv1.Task) bool {
	return task.Run != nil &&
		len(task.Run.Command) >= 2 &&
		filepath.Base(task.Run.Command[0]) == "goobers" &&
		task.Run.Command[1] == "backlog-query"
}

// prLifecycleBaseCommands are the goobers CLI subcommands whose "base" input
// resolves, at runtime, to the gaggle's own branch via providerBaseBranch()
// (cmd/goobers/providercmd.go, #2087) rather than a hardcoded "main" — the
// same 13 call sites providerInput("base", providerBaseBranch()) covers.
var prLifecycleBaseCommands = map[string]bool{
	"apply-verdict":            true,
	"check-fail-first":         true,
	"elect-lander":             true,
	"gate-removal-guard":       true,
	"gather-implement-context": true,
	"gather-pr-context":        true,
	"gather-sibling-context":   true,
	"issue-close-out":          true,
	"open-pr":                  true,
	"pr-select":                true,
	"rebase-pr":                true,
	"remediation-checkpoint":   true,
	"update-behind-pr":         true,
}

// checkPRLifecycleBaseBranch flags a PR-lifecycle task whose "base" input
// disagrees with the gaggle's own resolved branch (GaggleSpec.Project.
// Branch, "main" when unset, matching RepoRef's own default; #2088,
// sequenced after #2087 so the runtime default this check compares against
// is the derived branch, not the bare literal "main"). A dynamic base
// (inputsFrom) is resolved at runtime from an upstream stage's output — not
// statically checkable, so it is skipped rather than flagged. A task that
// declares no base input at all is likewise not flagged: since #2087,
// omitting it resolves correctly at runtime via providerBaseBranch() for any
// gaggle branch, so silence is not a bug — only a literal value that
// disagrees with the gaggle's real branch is.
func (ix *index) checkPRLifecycleBaseBranch(r *Report, w apiv1.Workflow, file string) {
	gaggle, ok := ix.gaggles[w.Spec.Gaggle]
	if !ok {
		return
	}
	resolvedBranch := gaggle.Spec.Project.Branch
	if resolvedBranch == "" {
		resolvedBranch = "main"
	}
	for _, t := range w.Spec.Tasks {
		if t.Run == nil || len(t.Run.Command) < 2 || filepath.Base(t.Run.Command[0]) != "goobers" {
			continue
		}
		if !prLifecycleBaseCommands[t.Run.Command[1]] {
			continue
		}
		base, declared := t.Inputs["base"]
		if !declared {
			continue
		}
		if _, dynamic := t.InputsFrom["base"]; dynamic {
			continue
		}
		if base != resolvedBranch {
			r.add(warningPRLifecycleBaseDrift, Warning, file, "Workflow", w.Name,
				"task %q declares base %q, but gaggle %q resolves to branch %q",
				t.Name, base, w.Spec.Gaggle, resolvedBranch)
		}
	}
}

func splitLabelInput(value string) []string {
	var labels []string
	for _, label := range strings.Split(value, ",") {
		if label = strings.TrimSpace(label); label != "" {
			labels = append(labels, label)
		}
	}
	return labels
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
	r.add(errorPreviewAnnotation, Error, ix.manifestFile[manifest.Name], "Manifest", manifest.Name,
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
			r.add(errorCICommand, Error, ix.gaggleFile[name], "Gaggle", name,
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
			r.add(errorBranchNamespace, Error, ix.gaggleFile[name], "Gaggle", name,
				"spec.branchNamespace %q %s, which would produce an invalid git run-branch name at runtime", ns, bad)
		}
	}
}

// checkGaggleSiblingLabelOverlap implements MIRC-2's (#1901) sibling-overlap
// validation warning: for each gaggle that declares Siblings, compare this
// gaggle's own effective requireLabels (a workflow's own task-level override
// when declared, else the gaggle's RequireLabels default — mirroring
// defaultBacklogQueryRequireLabels's runtime resolution exactly) against
// each declared sibling's given RequireLabels, but only when the sibling
// targets the SAME repo as this gaggle's own Project — repo identity is the
// sole match key (provider/baseUrl/owner/project/name), never gaggle name,
// per the design's explicit rejection of name-based matching (amended by
// #1908). A sibling targeting a different repo never triggers a warning,
// regardless of label similarity. Warn-only: this never fails validation,
// since the sibling's declared scope is this instance's own trusted
// assertion about another instance it cannot directly observe.
func (ix *index) checkGaggleSiblingLabelOverlap(r *Report) {
	for name, g := range ix.gaggles {
		if len(g.Spec.Siblings) == 0 {
			continue
		}
		file := ix.gaggleFile[name]

		type scope struct {
			workflow string
			labels   []string
		}
		var scopes []scope
		for identity, indexed := range ix.workflows {
			if identity.gaggle != name {
				continue
			}
			for _, task := range indexed.definition.Spec.Tasks {
				if !isBacklogQueryTask(task) {
					continue
				}
				labels := g.Spec.RequireLabels
				if v, overridden := task.Inputs["requireLabels"]; overridden {
					labels = splitLabelInput(v)
				}
				scopes = append(scopes, scope{workflow: identity.name, labels: labels})
			}
		}
		if len(scopes) == 0 {
			// No backlog-query task anywhere in this gaggle yet — still check
			// the bare gaggle-level default so a sibling misconfiguration
			// surfaces before any workflow adopts it.
			scopes = append(scopes, scope{labels: g.Spec.RequireLabels})
		}

		for _, sib := range g.Spec.Siblings {
			if !sameRepo(sib.Project, g.Spec.Project) {
				continue
			}
			for _, sc := range scopes {
				siblingDesc := sib.Label
				if siblingDesc == "" {
					siblingDesc = fmt.Sprintf("%s/%s/%s", sib.Project.Provider, sib.Project.Owner, sib.Project.Name)
				}
				where := "spec.requireLabels"
				if sc.workflow != "" {
					where = fmt.Sprintf("workflow %q's effective requireLabels", sc.workflow)
				}
				if len(sc.labels) == 0 {
					r.addWarning(WarningSiblingLabelOverlap, file, name, "Gaggle", name,
						"%s is empty, so this gaggle has no label partition from declared sibling %q — both target %s/%s/%s, allowing either instance to claim the same item",
						where, siblingDesc, sib.Project.Provider, sib.Project.Owner, sib.Project.Name)
					continue
				}
				overlap := intersectLabels(sc.labels, sib.RequireLabels)
				if len(overlap) == 0 {
					continue
				}
				r.addWarning(WarningSiblingLabelOverlap, file, name, "Gaggle", name,
					"%s %v overlaps declared sibling %q's requireLabels %v on shared label(s) %v — both target %s/%s/%s, so an item carrying %v could be independently claimed by either instance",
					where, sc.labels, siblingDesc, sib.RequireLabels, overlap, sib.Project.Provider, sib.Project.Owner, sib.Project.Name, overlap)
			}
		}
	}
}

// sameRepo reports whether a and b identify the same target repository —
// the sole sibling match key (MIRC-2, #1901, amended by #1908): gaggle/
// instance name carries zero cross-instance meaning, so it is never part of
// this comparison. BaseURL is included alongside provider/owner/project/name
// so two distinct self-hosted Gitea instances that happen to share an
// owner/name never collide.
func sameRepo(a, b apiv1.RepoRef) bool {
	return a.Provider == b.Provider &&
		a.BaseURL == b.BaseURL &&
		a.Owner == b.Owner &&
		a.Project == b.Project &&
		a.Name == b.Name
}

// intersectLabels returns the labels present in both a and b, sorted for a
// deterministic diagnostic message.
func intersectLabels(a, b []string) []string {
	inA := make(map[string]bool, len(a))
	for _, label := range a {
		inA[label] = true
	}
	seen := make(map[string]bool)
	var out []string
	for _, label := range b {
		if inA[label] && !seen[label] {
			seen[label] = true
			out = append(out, label)
		}
	}
	sort.Strings(out)
	return out
}

func (ix *index) checkGaggleRunControls(r *Report) {
	for name, g := range ix.gaggles {
		if g.Spec.RunControls == nil {
			continue
		}
		if err := runcontrol.Validate("spec.runControls", *g.Spec.RunControls); err != nil {
			r.add(errorRunControls, Error, ix.gaggleFile[name], "Gaggle", name, "%v", err)
		}
	}
}

// checkGaggleCheckout validates every declared repo checkout block's sparse
// cones (#649): the local runner now honors project.checkout.sparse by
// materializing a cone-mode sparse checkout, so a malformed declaration is a
// real misconfiguration caught here rather than a silently-inert notice.
func (ix *index) checkGaggleCheckout(r *Report) {
	for name, g := range ix.gaggles {
		file := ix.gaggleFile[name]
		check := func(field string, checkout *apiv1.CheckoutSpec) {
			if checkout == nil {
				return
			}
			if len(checkout.Sparse) == 0 {
				r.add(errorGaggleCheckoutSparse, Error, file, "Gaggle", name,
					"%s.sparse must declare at least one cone (omit checkout entirely for a full checkout)", field)
				return
			}
			seen := make(map[string]bool, len(checkout.Sparse))
			for i, cone := range checkout.Sparse {
				if reason := invalidSparseCone(cone); reason != "" {
					r.add(errorGaggleCheckoutSparse, Error, file, "Gaggle", name,
						"%s[%d] %q is not a valid sparse-checkout cone: %s", field, i, cone, reason)
					continue
				}
				if seen[cone] {
					r.add(errorGaggleCheckoutSparse, Error, file, "Gaggle", name,
						"%s[%d] duplicates cone %q", field, i, cone)
					continue
				}
				seen[cone] = true
			}
		}
		check("spec.project.checkout", g.Spec.Project.Checkout)
		for i := range g.Spec.AdditionalRepos {
			check(fmt.Sprintf("spec.additionalRepos[%d].checkout", i), g.Spec.AdditionalRepos[i].Checkout)
		}
	}
}

// invalidSparseCone reports why cone cannot be a git cone-mode sparse-checkout
// pattern, or "" if it can. Cone mode (`git sparse-checkout set --cone`)
// accepts only repo-relative directory prefixes — no glob patterns, no
// absolute paths, no lexical traversal outside the repo.
func invalidSparseCone(cone string) string {
	if cone == "" {
		return "must not be empty"
	}
	if path.IsAbs(cone) {
		return "must be repo-relative, not absolute"
	}
	if strings.Contains(cone, "\\") {
		return "must use forward slashes"
	}
	if cone == "." || cone == ".." {
		return `must not be "." or ".."`
	}
	if strings.ContainsAny(cone, "*?[]!") {
		return "cone mode does not support glob patterns; declare a directory prefix instead"
	}
	for _, segment := range strings.Split(cone, "/") {
		switch segment {
		case "":
			return "must not contain empty path segments (e.g. a leading, trailing, or doubled slash)"
		case "..":
			return `must not contain ".." segments`
		}
	}
	return ""
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
			r.add(errorConnectionReference, Error, file, "Gaggle", name,
				"%s names connection %q, but no Connection/%s is declared in the Manifest", field, ref, ref)
		}
		check(g.Spec.Project.ConnectionRef, "spec.project.connectionRef")
		check(g.Spec.Backlog.ConnectionRef, "spec.backlog.connectionRef")
		for i, repo := range g.Spec.AdditionalRepos {
			check(repo.ConnectionRef, fmt.Sprintf("spec.additionalRepos[%d].connectionRef", i))
		}
	}
}

// checkGaggleAdditionalRepos enforces read-only reference-repo coherence
// (MGV-10, #1285): a gaggle's AdditionalRepos are read-only reference sources,
// so an entry must not name the same repository as the gaggle's read-write
// Project — a repo cannot be both the write sink and a read-only reference. It
// also flags a reference repo listed twice, which is a redundant config.
func (ix *index) checkGaggleAdditionalRepos(r *Report) {
	for name, g := range ix.gaggles {
		file := ix.gaggleFile[name]
		seen := map[string]bool{}
		for i, repo := range g.Spec.AdditionalRepos {
			id := repoIdentity(repo)
			if id == repoIdentity(g.Spec.Project) {
				r.add(errorAdditionalRepoProject, Error, file, "Gaggle", name,
					"spec.additionalRepos[%d] names the same repository as spec.project (%s); a read-only reference repo must not be the gaggle's read-write project", i, id)
				continue
			}
			if seen[id] {
				r.add(errorAdditionalRepoDuplicate, Error, file, "Gaggle", name,
					"spec.additionalRepos[%d] repeats repository %s already listed in spec.additionalRepos", i, id)
				continue
			}
			seen[id] = true
		}
	}
}

// repoIdentity is the provider-qualified identity of a repo reference, used to
// compare a gaggle's Project against its AdditionalRepos. Branch and
// connectionRef are deliberately excluded — the same repo on a different branch
// or connection is still the same repo for read-only-vs-write-sink purposes.
func repoIdentity(ref apiv1.RepoRef) string {
	return strings.Join([]string{string(ref.Provider), ref.Owner, ref.Project, ref.Name}, "/")
}

func (ix *index) checkWorkflow(r *Report, w apiv1.Workflow, file string, allowPreview bool) {
	if _, ok := ix.gaggles[w.Spec.Gaggle]; !ok {
		ix.referenceNotFound(r, errorWorkflowGaggleReference, file, "Workflow", w.Name, "spec.gaggle names %q, but no Gaggle/%s definition was found",
			w.Spec.Gaggle, w.Spec.Gaggle)
	}
	r.addFeatureDiagnostics(file, w.Spec.Gaggle, "Workflow", w.Name,
		wf.CheckWorkflowFeatureSupport(wf.Definition{
			Name: w.Name, Version: 1, DSLVersion: w.DSLVersion, Spec: w.Spec,
		}, allowPreview))
	if err := runcontrol.ValidateWorkflow(w.Spec); err != nil {
		r.add(errorRunControls, Error, file, "Workflow", w.Name, "%v", err)
	}

	states := map[string]bool{}
	for _, t := range w.Spec.Tasks {
		if states[t.Name] {
			r.add(errorDuplicateState, Error, file, "Workflow", w.Name, "duplicate state name %q", t.Name)
		}
		states[t.Name] = true
	}
	for _, g := range w.Spec.Gates {
		if states[g.Name] {
			r.add(errorDuplicateState, Error, file, "Workflow", w.Name, "duplicate state name %q", g.Name)
		}
		states[g.Name] = true
	}

	for _, p := range w.Spec.Parallels {
		if states[p.Name] {
			r.add(errorDuplicateState, Error, file, "Workflow", w.Name, "duplicate state name %q", p.Name)
		}
		states[p.Name] = true
	}

	if w.Spec.Start != "" && !states[w.Spec.Start] {
		r.add(errorStartState, Error, file, "Workflow", w.Name, "start state %q is not a defined task or gate", w.Spec.Start)
	}

	// Docs-location surface (#1016): a declared docs root must be a usable
	// repo-relative containment root. This is the config-load lexical half —
	// empty / absolute / escaping / whole-repo roots are rejected here, with the
	// same clear message the runtime boundary would carry. A root's existence in
	// the repository is a separate filesystem check the `goobers validate` CLI
	// layers on top (validate.go), since api-level validation has no repo tree.
	for i, dr := range w.Spec.DocsRoots {
		if err := configboundary.ValidateDocsRoot(dr); err != nil {
			r.add(errorDocsRoot, Error, file, "Workflow", w.Name, "spec.docsRoots[%d]: %v", i, err)
		}
	}

	// Tutor topology (TUT-A4, Tutor v2 design doc §4.3): a per-workflow
	// tutor's target must be explicit and must name a real workflow in the
	// SAME gaggle — a tutor confined to another gaggle's workflow would defeat
	// the hard silo Gaggle already establishes for this definition itself. A
	// per-gaggle tutor has no target (the whole gaggle is already its scope).
	if ts := w.Spec.TutorScope; ts != nil {
		switch ts.Tier {
		case apiv1.TutorScopePerWorkflow:
			switch ts.Target {
			case "":
				r.add(errorTutorScopeTarget, Error, file, "Workflow", w.Name, "spec.tutorScope.target is required when spec.tutorScope.tier is %q", ts.Tier)
			case w.Name:
				r.add(errorTutorScopeTarget, Error, file, "Workflow", w.Name, "spec.tutorScope.target %q must not name this workflow itself", ts.Target)
			default:
				if _, ok := ix.workflows[workflowIdentity{gaggle: w.Spec.Gaggle, name: ts.Target}]; !ok {
					ix.referenceNotFound(r, errorTutorScopeTarget, file, "Workflow", w.Name,
						"spec.tutorScope.target names %q, but no Workflow/%s definition was found in gaggle %q",
						ts.Target, ts.Target, w.Spec.Gaggle)
				}
			}
		case apiv1.TutorScopePerGaggle:
			if ts.Target != "" {
				r.add(errorTutorScopeTarget, Error, file, "Workflow", w.Name, "spec.tutorScope.target must be empty when spec.tutorScope.tier is %q, got %q", ts.Tier, ts.Target)
			}
		default:
			r.add(errorTutorScopeTarget, Error, file, "Workflow", w.Name, "spec.tutorScope.tier %q is not one of per-workflow, per-gaggle", ts.Tier)
		}
	}

	ix.checkPRLifecycleBaseBranch(r, w, file)

	for _, t := range w.Spec.Tasks {
		if t.Type == apiv1.TaskAgentic && t.Goober != "" {
			goober, ok := ix.goobers[t.Goober]
			switch {
			case !ok:
				ix.referenceNotFound(r, errorTaskGooberReference, file, "Workflow", w.Name, "task %q targets goober %q which is not defined", t.Name, t.Goober)
			case goober.Spec.Gaggle != w.Spec.Gaggle:
				r.add(errorTaskGooberGaggle, Error, file, "Workflow", w.Name,
					"task %q targets goober %q in gaggle %q, not workflow gaggle %q",
					t.Name, t.Goober, goober.Spec.Gaggle, w.Spec.Gaggle)
			}
		}
		if t.Next != "" && !wf.IsReservedAnyTarget(t.Next) && !states[t.Next] {
			r.add(errorTaskNextState, Error, file, "Workflow", w.Name, "task %q next state %q is not defined", t.Name, t.Next)
		}
	}

	for _, g := range w.Spec.Gates {
		ix.checkGateEvaluator(r, w, g, file)
		if g.Evaluator == apiv1.EvaluatorAgentic && g.Agentic != nil && g.Agentic.Goober != "" {
			goober, ok := ix.goobers[g.Agentic.Goober]
			switch {
			case !ok:
				ix.referenceNotFound(r, errorGateGooberReference, file, "Workflow", w.Name, "gate %q reviewer goober %q is not defined", g.Name, g.Agentic.Goober)
			case goober.Spec.Gaggle != w.Spec.Gaggle:
				r.add(errorGateGooberGaggle, Error, file, "Workflow", w.Name,
					"gate %q reviewer goober %q is in gaggle %q, not workflow gaggle %q",
					g.Name, g.Agentic.Goober, goober.Spec.Gaggle, w.Spec.Gaggle)
			}
		}
		for outcome, next := range g.Branches {
			// Empty means the success terminal (TerminalComplete); "@abort"
			// and "@escalate" are reserved terminal targets and "@join" is a
			// reserved branch target — none is a dangling reference
			// (workflow.IsReservedAnyTarget).
			if next != "" && !wf.IsReservedAnyTarget(next) && !states[next] {
				r.add(errorGateBranch, Error, file, "Workflow", w.Name, "gate %q branch %q -> %q is not a defined state", g.Name, outcome, next)
			}
		}
	}

	// Delegate the deeper semantic analysis to the workflow compiler so the CLI
	// and the compiler stay in lockstep: reachability + loop-without-exit,
	// schedule-expression validity, and capability/harness admission. These are
	// checks the inline field-by-field pass above deliberately does not duplicate.
	def := wf.Definition{Name: w.Name, Version: 1, DSLVersion: w.DSLVersion, Spec: w.Spec}
	for _, msg := range wf.CheckWarnings(def) {
		if ix.acknowledgesManualOnly(w, msg) {
			continue
		}
		r.addWarning(WarningCompatibility, file, w.Spec.Gaggle, "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckReachability(def) {
		r.add(errorReachability, Error, file, "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckSchedules(def) {
		r.add(errorSchedule, Error, file, "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckGateOutcomes(def) {
		r.add(errorGateOutcome, Error, file, "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckGateParameters(def) {
		r.add(errorGateParameter, Error, file, "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckTriggerFields(def) {
		r.add(errorTriggerField, Error, file, "Workflow", w.Name, "%s", msg)
	}
	for _, msg := range wf.CheckWorkflowAdmission(def, ix.gooberSpecs()) {
		r.add(errorWorkflowAdmission, Error, file, "Workflow", w.Name, "%s", msg)
	}
	ix.checkCapabilityRuntimeSupport(r, w, file)
	// Stage output/input contracts (#900). These catch the class of defect
	// that is structurally valid, compiles, and then silently loses data at
	// runtime — a stage promising outputs it has no channel to emit, or
	// reading an upstream output the stage actually preceding it on some
	// branch does not produce. Reported as errors: both are unconditionally
	// broken at runtime, on some path, every time.
	for _, msg := range wf.CheckStageContracts(def) {
		r.add(errorStageContract, Error, file, "Workflow", w.Name, "%s", msg)
	}
	// Path simulation (#913, Tier 2 of the assurance ladder #903). Walks the
	// compiled machine over every combination of gate outcomes, tracking what
	// the immediately preceding task actually emits on each concrete path —
	// catching an inputsFrom handoff that only breaks along one sequence of
	// outcomes, which CheckStageContracts' per-edge union above cannot
	// express, and reporting the exact path as evidence.
	for _, msg := range wf.CheckPathSimulation(def) {
		r.add(errorPathSimulation, Error, file, "Workflow", w.Name, "%s", msg)
	}
	// Required-input contracts (#1061). The input-side analog of the above:
	// a deterministic stage that invokes a `goobers` subcommand without
	// wiring an input that subcommand hard-requires. This is what a
	// hand-maintained instance config drifting behind the binary produces —
	// merge-review's apply-verdict losing its selectedHeadSha wiring stalled
	// every election for a full build, and nothing static caught it. Also an
	// error: the stage fails on every run, unconditionally.
	for _, msg := range wf.CheckStageRequiredInputs(def) {
		r.add(errorStageRequiredInput, Error, file, "Workflow", w.Name, "%s", msg)
	}
	// Bounded waits must finish before the executor can terminate their stage;
	// command-specific clamps are modeled by the workflow check itself.
	for _, msg := range wf.CheckStageTimeoutCoherence(def) {
		r.add(errorStageTimeout, Error, file, "Workflow", w.Name, "%s", msg)
	}
	// Only the breaking half is reported here. CheckStageContractWarnings
	// covers the same omission on outputs nothing reads yet, which #881's
	// VER003 "expectedOutputs is declared but not enforced" already warns
	// about for every such stage — emitting both would put two warnings on
	// one missing line. It stays exported for callers that want the strict
	// bar (this repo holds its own shipped workflows to it in
	// internal/workflow's stage-contract test).
}

func (ix *index) checkCapabilityRuntimeSupport(r *Report, w apiv1.Workflow, file string) {
	gaggle, ok := ix.gaggles[w.Spec.Gaggle]
	if !ok || len(gaggle.Spec.AdditionalRepos) == 0 {
		return
	}
	for _, task := range w.Spec.Tasks {
		if !hasCapability(task.Capabilities, capability.ContentsRead) || effectiveTaskWorkspace(task) != apiv1.WorkspaceScratch {
			continue
		}
		r.add(errorCapabilityRuntimeSupport, Error, file, "Workflow", w.Name,
			"task %q declares capability %q in a scratch workspace, but Gaggle/%s additionalRepos are only provisioned for repo-backed workspaces",
			task.Name, capability.ContentsRead, w.Spec.Gaggle)
	}
}

func hasCapability(declared []string, wanted capability.Capability) bool {
	for _, value := range declared {
		if value == string(wanted) {
			return true
		}
	}
	return false
}

func effectiveTaskWorkspace(task apiv1.Task) apiv1.WorkspaceMode {
	if task.Run != nil && task.Run.Workspace != "" {
		return task.Run.Workspace
	}
	if task.Workspace != "" {
		return task.Workspace
	}
	return apiv1.WorkspaceRepo
}

func (ix *index) acknowledgesManualOnly(w apiv1.Workflow, warning string) bool {
	if len(ix.manifests) != 1 || ix.manifests[0].Annotations[acknowledgeManualOnlyAnnotation] != "true" {
		return false
	}
	if len(w.Spec.Triggers) != 1 || w.Spec.Triggers[0].Type != apiv1.TriggerManual {
		return false
	}
	want := fmt.Sprintf(
		"workflow %q has no schedule trigger; it will not fire autonomously — run it with `goobers run %s`",
		w.Name,
		w.Name,
	)
	return warning == want
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
		r.add(errorGateEvaluatorCardinality, Error, file, "Workflow", w.Name, "gate %q must have exactly one evaluator block, found %d", g.Name, set)
		return
	}
	mismatch := (g.Evaluator == apiv1.EvaluatorAutomated && g.Automated == nil) ||
		(g.Evaluator == apiv1.EvaluatorAgentic && g.Agentic == nil) ||
		(g.Evaluator == apiv1.EvaluatorHuman && g.Human == nil)
	if mismatch {
		r.add(errorGateEvaluatorMismatch, Error, file, "Workflow", w.Name, "gate %q evaluator=%q but the matching evaluator block is not set", g.Name, g.Evaluator)
	}
}
