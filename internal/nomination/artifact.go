// Package nomination is the deterministic issue-write executor for the
// nomination workflows — TBH-1 #2251's first slice, built on the decomposition
// binding (internal/decomposition: typed artifact, digest-bound validator and
// publisher, publisher-owned trust labels) rather than the #2225 proposal
// runtime, which does not exist yet.
//
// The finder (an agentic stage holding no issue credential) emits a
// goobers.dev/nominations/v1 artifact; `goobers file-issues --check`
// validates it and runs the read-only dedupe scan; `goobers file-issues`
// creates the issues. The model proposes area/type labels and evidence; every
// goobers:* label, the dedupe decision, the budget and the approval decision
// belong to the publisher. goobers:approved (the SEC-047 trust label) is
// applied on one condition only — engagement decision 004: the nomination
// names a finding that the deterministic collect-repo-signals stage's own
// tool output contains byte for byte (see Findings and Policy.AutoApprove);
// nothing the model writes can satisfy that on its own.
package nomination

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/goobers/goobers/providers"
)

// SchemaV1 is the only nominations artifact schema this build reads.
const SchemaV1 = "goobers.dev/nominations/v1"

// FlakeLabel is the label the flake-watch GitHub Action puts on every issue
// it owns (test/flakeledger). The publisher never applies a goobers label to
// an issue carrying it: flakeledger strips them on its next hourly refresh,
// so the write would be a silent no-op that looks like success.
const FlakeLabel = "ci:flake"

// controlMarkerPrefix is the prefix of every HTML-comment marker goobers
// stage code parses out of an issue body (nomination keys, filed-by and seen
// markers, flake fingerprints, decomposition records). A model-authored field
// containing it could forge a dedupe key, a flake fingerprint, or another
// run's ownership of an issue, so validation rejects it.
const controlMarkerPrefix = "<!-- goobers-"

// rejectedControlText is every piece of control text goobers parses out of
// an issue body: the HTML-comment markers above and the provider's run-id
// footer, which GitHubProvider.CreateWorkItem treats as proof that a run
// already created the issue — a nomination body carrying a sibling's footer
// would collapse two nominations onto one issue. Every model-authored string
// the publisher renders into a body or comment is checked against it.
var rejectedControlText = []string{controlMarkerPrefix, providers.RunIDFooterPrefix}

var (
	keyMarker             = regexp.MustCompile(`<!-- goobers-nomination-key:([0-9a-f]{64}) -->`)
	seenMarker            = regexp.MustCompile(`<!-- goobers-nomination-seen:([0-9a-f]{64}) run=([^ >]+) -->`)
	filedMarker           = regexp.MustCompile(`<!-- goobers-nomination-filed:([0-9a-f]{64}) run=([^ >]+) -->`)
	findingMarker         = regexp.MustCompile(`<!-- goobers-nomination-finding:([0-9a-f]{64}) -->`)
	flakeFingerprintMark  = regexp.MustCompile(`<!-- goobers-flake-fingerprint:([0-9a-f]{64}) -->`)
	artifactDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	keyPattern            = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
)

// RiskClass is the finder's risk assessment of one nomination. It is rendered
// into the issue; only RiskLow can be auto-approved (and only on a confirmed
// tool finding), and RiskHuman always files as goobers:needs-human.
type RiskClass string

// Risk classes.
const (
	RiskLow      RiskClass = "low"
	RiskStandard RiskClass = "standard"
	RiskHuman    RiskClass = "human"
)

// EvidenceKind is the kind of one evidence pointer.
type EvidenceKind string

// Evidence kinds. A journal pointer names a run event; an artifact pointer
// names a stage artifact by path and content digest; a source pointer names a
// source location; a finding pointer names a deterministic tool finding
// (Finding) the publisher confirms byte for byte against the
// collect-repo-signals stdout artifact of this run. Only a confirmed finding
// counts for anything beyond the maintainer's eyes: it is the one route to
// goobers:approved and the one thing that orders the filing budget. The
// other three kinds are pointers for the maintainer, not proof.
const (
	EvidenceJournal  EvidenceKind = "journal"
	EvidenceArtifact EvidenceKind = "artifact"
	EvidenceSource   EvidenceKind = "source"
	EvidenceFinding  EvidenceKind = "finding"
)

// Evidence is one pointer backing a nomination. A finding pointer carries
// Tool plus, for vet and lint, Path, Line and Rule (the vet diagnostic text /
// the linter name), or, for test, Package and Test — exactly the fields of
// the tool's own record, which the publisher compares byte for byte.
type Evidence struct {
	Kind   EvidenceKind `json:"kind"`
	RunID  string       `json:"runId,omitempty"`
	Seq    uint64       `json:"seq,omitempty"`
	Path   string       `json:"path,omitempty"`
	Digest string       `json:"digest,omitempty"`
	Line   int          `json:"line,omitempty"`
	// Finding fields (kind finding only).
	Tool    Tool   `json:"tool,omitempty"`
	Rule    string `json:"rule,omitempty"`
	Package string `json:"package,omitempty"`
	Test    string `json:"test,omitempty"`
}

// TestFailure identifies a test failure the nomination is about, in the
// exact (package, test, signature) shape flake-watch fingerprints, so the
// publisher can exclude anything flake-watch already owns.
type TestFailure struct {
	Package   string `json:"package"`
	Test      string `json:"test"`
	Signature string `json:"signature"`
}

// Nomination is one proposed issue.
type Nomination struct {
	Key       string `json:"key"`
	DedupeKey string `json:"dedupeKey"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	// Labels may name area:* and type:* labels only; every goobers:* label
	// is publisher-owned and cannot be requested by the model.
	Labels     []string  `json:"labels,omitempty"`
	RiskClass  RiskClass `json:"riskClass"`
	RiskReason string    `json:"riskReason"`
	// RequiresHumanReview carries the source finding's
	// nomination_guardrails.requires_human_review flag.
	RequiresHumanReview bool         `json:"requiresHumanReview,omitempty"`
	Evidence            []Evidence   `json:"evidence"`
	TestFailure         *TestFailure `json:"testFailure,omitempty"`
}

// Producer names the stage that emitted the artifact.
type Producer struct {
	Stage   string `json:"stage"`
	Attempt int    `json:"attempt"`
}

// Artifact is the goobers.dev/nominations/v1 artifact. RunID must name the
// run the finder ran in (its GOOBERS_RUN_ID): Validate refuses any other
// value, so an issue's provenance and the retry read-back both key on the
// stage's own run id, never on a model-authored one.
type Artifact struct {
	Schema      string       `json:"schema"`
	RunID       string       `json:"runId"`
	Producer    Producer     `json:"producer"`
	Nominations []Nomination `json:"nominations"`
}

// ValidationResult is Validate's outcome.
type ValidationResult struct {
	Valid         bool
	Errors        []string
	SchemaInvalid bool
}

// Validate checks every closed-artifact rule against the run the stage runs
// in (runID, the stage's GOOBERS_RUN_ID). It accumulates findings rather than
// failing on the first, except for an unsupported schema, which is the one
// fail-fast case: the rest of the shape cannot be trusted.
func Validate(artifact Artifact, runID string) ValidationResult {
	if artifact.Schema != SchemaV1 {
		return ValidationResult{
			Errors:        []string{fmt.Sprintf("unsupported or malformed nominations schema %q (want %q)", artifact.Schema, SchemaV1)},
			SchemaInvalid: true,
		}
	}
	var errs []string
	switch {
	case strings.TrimSpace(artifact.RunID) == "":
		errs = append(errs, "artifact names no runId")
	case artifact.RunID != runID:
		// Mirrors decomposition's selection binding: the artifact must be the
		// one this run's finder produced, not one carried in from elsewhere.
		errs = append(errs, fmt.Sprintf("artifact names run %q but this stage runs as %q", artifact.RunID, runID))
	}
	if strings.TrimSpace(artifact.Producer.Stage) == "" {
		errs = append(errs, "artifact names no producer stage")
	} else if text, found := containsControlText(artifact.Producer.Stage); found {
		errs = append(errs, fmt.Sprintf("artifact producer stage contains goobers control text %q", text))
	}
	keys := make(map[string]bool, len(artifact.Nominations))
	dedupeKeys := make(map[string]string, len(artifact.Nominations))
	for i, n := range artifact.Nominations {
		where := fmt.Sprintf("nomination %d", i)
		if n.Key != "" {
			where = fmt.Sprintf("nomination %q", n.Key)
		}
		if !keyPattern.MatchString(n.Key) {
			errs = append(errs, where+" has a missing or malformed key (want ^[a-z0-9][a-z0-9._-]{0,127}$)")
		} else if keys[n.Key] {
			errs = append(errs, fmt.Sprintf("duplicate nomination key %q", n.Key))
		}
		keys[n.Key] = true
		errs = append(errs, validateNomination(where, n, dedupeKeys)...)
	}
	return ValidationResult{Valid: len(errs) == 0, Errors: errs}
}

func validateNomination(where string, n Nomination, dedupeKeys map[string]string) []string {
	var errs []string
	switch {
	case strings.TrimSpace(n.DedupeKey) == "":
		errs = append(errs, where+" has an empty dedupeKey")
	case strings.ContainsAny(n.DedupeKey, "\r\n") || len(n.DedupeKey) > 256:
		errs = append(errs, where+" has a malformed dedupeKey (single line, at most 256 bytes)")
	default:
		if prior, seen := dedupeKeys[n.DedupeKey]; seen {
			errs = append(errs, fmt.Sprintf("%s repeats the dedupeKey of nomination %q", where, prior))
		} else {
			dedupeKeys[n.DedupeKey] = n.Key
		}
	}
	if strings.TrimSpace(n.Title) == "" {
		errs = append(errs, where+" has an empty title")
	} else if len(n.Title) > 200 || strings.ContainsAny(n.Title, "\r\n") {
		errs = append(errs, where+" has a malformed title (single line, at most 200 bytes)")
	}
	if len(strings.TrimSpace(n.Body)) < 20 {
		errs = append(errs, where+" body is too short to describe a defect")
	}
	if strings.TrimSpace(n.RiskReason) == "" {
		errs = append(errs, where+" gives no riskReason")
	}
	switch n.RiskClass {
	case RiskLow, RiskStandard, RiskHuman:
	default:
		errs = append(errs, fmt.Sprintf("%s has riskClass %q (want low, standard, or human)", where, n.RiskClass))
	}
	// Every model-authored string the publisher renders verbatim into an
	// issue body or comment is checked, not only the prose fields: an
	// evidence path or a test name is body text too.
	rendered := []struct{ name, value string }{
		{"title", n.Title}, {"body", n.Body}, {"riskReason", n.RiskReason}, {"dedupeKey", n.DedupeKey},
	}
	for i, e := range n.Evidence {
		rendered = append(rendered,
			struct{ name, value string }{fmt.Sprintf("evidence %d path", i), e.Path},
			struct{ name, value string }{fmt.Sprintf("evidence %d runId", i), e.RunID},
			struct{ name, value string }{fmt.Sprintf("evidence %d rule", i), e.Rule},
			struct{ name, value string }{fmt.Sprintf("evidence %d package", i), e.Package},
			struct{ name, value string }{fmt.Sprintf("evidence %d test", i), e.Test})
	}
	if n.TestFailure != nil {
		rendered = append(rendered,
			struct{ name, value string }{"testFailure package", n.TestFailure.Package},
			struct{ name, value string }{"testFailure test", n.TestFailure.Test},
			struct{ name, value string }{"testFailure signature", n.TestFailure.Signature})
	}
	for _, field := range rendered {
		if text, found := containsControlText(field.value); found {
			errs = append(errs, fmt.Sprintf("%s %s contains goobers control text %q, which could forge a dedupe key, a flake fingerprint, an issue's filing run, or a create idempotency footer", where, field.name, text))
		}
	}
	errs = append(errs, validateLabels(where, n.Labels)...)
	if len(n.Evidence) == 0 {
		errs = append(errs, where+" carries no evidence")
	}
	for i, e := range n.Evidence {
		errs = append(errs, validateEvidence(fmt.Sprintf("%s evidence %d", where, i), e)...)
	}
	if n.TestFailure != nil {
		if strings.TrimSpace(n.TestFailure.Package) == "" || strings.TrimSpace(n.TestFailure.Test) == "" {
			errs = append(errs, where+" testFailure needs both package and test")
		}
	}
	return errs
}

// containsControlText reports the first piece of rejectedControlText a
// model-authored value carries.
func containsControlText(value string) (string, bool) {
	for _, text := range rejectedControlText {
		if strings.Contains(value, text) {
			return text, true
		}
	}
	return "", false
}

// validateLabels enforces the same rule decomposition enforces on child
// labels (internal/decomposition/plan.go's disallowedChildLabelPrefixes): the
// model may propose area:* and type:* labels and nothing else. Exactly one
// type:* is required so the publisher never files an untyped issue.
func validateLabels(where string, labels []string) []string {
	var errs []string
	types, areas := 0, 0
	seen := make(map[string]bool, len(labels))
	for _, label := range labels {
		if seen[label] {
			errs = append(errs, fmt.Sprintf("%s repeats label %q", where, label))
			continue
		}
		seen[label] = true
		text, control := containsControlText(label)
		switch {
		case control:
			// Checked before the allowlisted prefixes so a "type:<!-- goobers-"
			// label is refused rather than counted as the type label.
			errs = append(errs, fmt.Sprintf("%s label %q contains goobers control text %q", where, label, text))
		case strings.HasPrefix(label, "goobers:"), strings.HasPrefix(label, "goobers/status:"):
			errs = append(errs, fmt.Sprintf("%s requests publisher-owned label %q", where, label))
		case label == FlakeLabel:
			errs = append(errs, fmt.Sprintf("%s requests flake-watch's label %q", where, label))
		case strings.HasPrefix(label, "type:"):
			types++
		case strings.HasPrefix(label, "area:"):
			areas++
		default:
			errs = append(errs, fmt.Sprintf("%s requests non-allowlisted label %q (only area:* and type:* may be proposed)", where, label))
		}
	}
	if types != 1 {
		errs = append(errs, fmt.Sprintf("%s must carry exactly one type:* label, has %d", where, types))
	}
	if areas > 1 {
		errs = append(errs, fmt.Sprintf("%s must carry at most one area:* label, has %d", where, areas))
	}
	return errs
}

func validateEvidence(where string, e Evidence) []string {
	var errs []string
	switch e.Kind {
	case EvidenceJournal:
		if strings.TrimSpace(e.RunID) == "" || e.Seq == 0 {
			errs = append(errs, where+" (journal) needs runId and a positive seq")
		}
		if e.RunID != "" && path.Base(e.RunID) != e.RunID {
			errs = append(errs, where+" (journal) runId is not a single path segment")
		}
	case EvidenceArtifact:
		errs = append(errs, validateEvidencePath(where+" (artifact)", e.Path)...)
		if !artifactDigestPattern.MatchString(e.Digest) {
			errs = append(errs, where+" (artifact) digest must be sha256:<64 hex>")
		}
	case EvidenceSource:
		errs = append(errs, validateEvidencePath(where+" (source)", e.Path)...)
		if e.Line < 0 {
			errs = append(errs, where+" (source) line must not be negative")
		}
	case EvidenceFinding:
		errs = append(errs, validateFindingEvidence(where+" (finding)", e)...)
	default:
		errs = append(errs, fmt.Sprintf("%s has kind %q (want journal, artifact, source, or finding)", where, e.Kind))
	}
	return errs
}

// validateFindingEvidence checks the shape of a finding pointer per tool: a
// vet or lint finding is a file, a positive line and a rule (the diagnostic
// text for vet, the linter name for lint); a test finding is a package and
// a test name. Fields of the other shape must be empty, so a pointer can
// never be read as two findings.
func validateFindingEvidence(where string, e Evidence) []string {
	var errs []string
	switch e.Tool {
	case ToolVet, ToolLint:
		errs = append(errs, validateEvidencePath(where, e.Path)...)
		if e.Line <= 0 {
			errs = append(errs, where+" line must be positive")
		}
		if strings.TrimSpace(e.Rule) == "" || strings.ContainsAny(e.Rule, "\r\n") {
			errs = append(errs, where+" rule must be a single non-empty line (the vet diagnostic text, or the golangci-lint linter name)")
		}
		if e.Package != "" || e.Test != "" {
			errs = append(errs, where+" names a package or test, which only a test finding carries")
		}
	case ToolTest:
		if strings.TrimSpace(e.Package) == "" || strings.TrimSpace(e.Test) == "" {
			errs = append(errs, where+" needs both package and test")
		}
		if strings.ContainsAny(e.Package+e.Test, " \r\n") {
			errs = append(errs, where+" package and test must be single tokens")
		}
		if e.Path != "" || e.Line != 0 || e.Rule != "" {
			errs = append(errs, where+" names a path, line or rule, which only a vet or lint finding carries")
		}
	default:
		errs = append(errs, fmt.Sprintf("%s has tool %q (want vet, lint, or test)", where, e.Tool))
	}
	if e.RunID != "" || e.Seq != 0 || e.Digest != "" {
		errs = append(errs, where+" carries journal or artifact fields")
	}
	return errs
}

func validateEvidencePath(where, p string) []string {
	if strings.TrimSpace(p) == "" {
		return []string{where + " names no path"}
	}
	if strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || path.Clean(p) != p || p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return []string{fmt.Sprintf("%s path %q must be a clean relative slash path inside the workspace", where, p)}
	}
	return nil
}

// Digest returns the canonical digest of an artifact — the value
// `file-issues --check` records and `file-issues` compares before writing,
// mirroring decomposition.PlanDigest.
func Digest(artifact Artifact) (string, error) {
	data, err := json.Marshal(artifact)
	if err != nil {
		return "", fmt.Errorf("marshal nominations artifact: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// KeyHash is the stable identity of a nomination across runs: the hex
// sha256 of its dedupeKey. It is what the body marker and the create
// idempotency key carry, so the model's free-form key never reaches an issue
// body verbatim.
func KeyHash(dedupeKey string) string {
	sum := sha256.Sum256([]byte(dedupeKey))
	return hex.EncodeToString(sum[:])
}

// KeyMarker is the first line of every issue body the publisher writes,
// mirroring flake-watch's <!-- goobers-flake-fingerprint:… --> marker.
func KeyMarker(hash string) string {
	return "<!-- goobers-nomination-key:" + hash + " -->"
}

// ParseKeyMarker extracts the nomination key hash from an issue body.
func ParseKeyMarker(body string) (string, bool) {
	m := keyMarker.FindStringSubmatch(body)
	if len(m) != 2 {
		return "", false
	}
	return m[1], true
}

// ParseFlakeFingerprint extracts flake-watch's fingerprint from an issue
// body, using the exact marker test/flakewatch writes.
func ParseFlakeFingerprint(body string) (string, bool) {
	m := flakeFingerprintMark.FindStringSubmatch(body)
	if len(m) != 2 {
		return "", false
	}
	return m[1], true
}

// SeenMarker is the idempotency marker of the occurrence comment the
// publisher appends to an open duplicate, once per (key, run).
func SeenMarker(hash, runID string) string {
	return "<!-- goobers-nomination-seen:" + hash + " run=" + runID + " -->"
}

// FiledMarker is the ownership marker the publisher writes into every issue
// body it creates: the run that filed the key. It is what a retried attempt
// reads back to find the issue it already created, so it is a control marker
// (rejected in every model-authored field) and never plain body text.
func FiledMarker(hash, runID string) string {
	return "<!-- goobers-nomination-filed:" + hash + " run=" + runID + " -->"
}

func hasFiledMarker(body, hash, runID string) bool {
	for _, m := range filedMarker.FindAllStringSubmatch(body, -1) {
		if len(m) == 3 && m[1] == hash && m[2] == runID {
			return true
		}
	}
	return false
}

// filedByRunForKeys reports whether body carries a filed marker for runID
// and one of keyHashes — an issue this run filed for a nomination of the
// artifact being filed (a retried attempt's read-back, or a sibling in the
// same artifact), as opposed to an issue any other run, or this run for an
// earlier artifact, filed.
func filedByRunForKeys(body, runID string, keyHashes map[string]bool) bool {
	for _, m := range filedMarker.FindAllStringSubmatch(body, -1) {
		if len(m) == 3 && m[2] == runID && keyHashes[m[1]] {
			return true
		}
	}
	return false
}

// FindingHash is the identity of a deterministic tool finding across runs:
// the hex sha256 of its exact tuple (tool, rule, path, line, package, test)
// — the same tuple Findings.Match compares byte for byte, so it is the
// finding as the tool records it, not as the model spells its dedupeKey.
func FindingHash(f Finding) string {
	sum := sha256.Sum256([]byte(f.key()))
	return hex.EncodeToString(sum[:])
}

// FindingMarker is the control marker the publisher writes into the body of
// every issue whose nomination names a finding, one per finding pointer. It
// is what the "no open or windowed-closed duplicate" approval bound is keyed
// on: a prior nominated issue carrying the same finding marker is the same
// defect, whatever dedupeKey the model wrote, so at most one issue per
// finding is ever approved.
func FindingMarker(hash string) string {
	return "<!-- goobers-nomination-finding:" + hash + " -->"
}

// ParseFindingMarkers extracts every finding hash an issue body carries, in
// body order, without duplicates.
func ParseFindingMarkers(body string) []string {
	var hashes []string
	seen := map[string]bool{}
	for _, m := range findingMarker.FindAllStringSubmatch(body, -1) {
		if len(m) == 2 && !seen[m[1]] {
			seen[m[1]] = true
			hashes = append(hashes, m[1])
		}
	}
	return hashes
}

func hasSeenMarker(body, hash, runID string) bool {
	for _, m := range seenMarker.FindAllStringSubmatch(body, -1) {
		if len(m) == 3 && m[1] == hash && m[2] == runID {
			return true
		}
	}
	return false
}
