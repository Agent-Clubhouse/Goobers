package decisiongate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/decider"
)

// ScopeThresholdKey is the threshold key for the reviewer-finding scope
// question.
const ScopeThresholdKey = "review_finding_in_scope"

// DefaultScopeThreshold keeps ambiguous findings on the existing path: only a
// confident "out of scope" answer becomes a would-demote record.
var DefaultScopeThreshold = Threshold{Accept: 0.8, Reject: 0.1}

// Bounds on what one scope check sends to the model and how many model calls
// it makes. Findings past the call cap are left untouched (unsure).
const (
	maxScopeText     = 2000
	maxScopeCriteria = 4000
	maxScopeFiles    = 200
	// MaxScopeModelCalls is the most model calls made for one verdict.
	MaxScopeModelCalls = 20
)

// ScopeOutcome is what a scope check says about one finding. Only
// ScopeWouldDemote is ever a candidate for advisory treatment, and in shadow
// mode nothing acts on it.
type ScopeOutcome string

// Scope outcomes. ScopeUnsure covers uncertain answers, model errors and
// findings past the call cap: the verdict stays untouched.
const (
	ScopeInScope     ScopeOutcome = "in-scope"
	ScopeWouldDemote ScopeOutcome = "would-demote"
	ScopeUnsure      ScopeOutcome = "unsure"
)

// ScopeSource says who decided an outcome.
type ScopeSource string

// Scope sources.
const (
	ScopeByCode  ScopeSource = "code"
	ScopeByModel ScopeSource = "model"
)

// ScopeFinding is the part of a reviewer finding the scope check needs.
type ScopeFinding struct {
	Severity string `json:"severity"`
	Category string `json:"category,omitempty"`
	// File is the file the finding points at, if any.
	File string `json:"file,omitempty"`
	Text string `json:"-"`
}

// ScopeFindingFromAPI adapts a published verdict finding. The category is the
// finding's class, falling back to its learning signature.
func ScopeFindingFromAPI(f apiv1.Finding) ScopeFinding {
	cat := string(f.Class)
	if cat == "" {
		cat = f.LearningSignature
	}
	file := f.Location
	if i := strings.LastIndex(file, ":"); i > 0 && isDigits(file[i+1:]) {
		file = file[:i]
	}
	return ScopeFinding{Severity: string(f.Severity), Category: cat, File: file, Text: f.Message}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ScopeInput is one review verdict's findings with the code-known context.
type ScopeInput struct {
	Findings []ScopeFinding
	// DiffFiles are the files the change under review touches.
	DiffFiles []string
	// PriorFindings are the findings of earlier rounds on the same change.
	PriorFindings []ScopeFinding
	// Criteria is the issue's acceptance criteria text.
	Criteria string
}

// ScopeFacts are the code-decided facts about one finding.
type ScopeFacts struct {
	Critical bool `json:"critical"`
	// Unfixable is true for categories the implementer cannot resolve
	// (rebase-needed, cross-pr-blocked).
	Unfixable bool `json:"unfixable"`
	// NamesDiffFile is true when the finding's file, or a path in its text, is
	// one of the changed files.
	NamesDiffFile bool `json:"namesDiffFile"`
	// OutsideDiff is true when the finding names a file and none is in the
	// diff.
	OutsideDiff bool `json:"outsideDiff"`
	// UnchangedFromPrior is true when an earlier round raised the same
	// finding.
	UnchangedFromPrior bool `json:"unchangedFromPrior"`
}

// ScopeFactsOf computes the facts for one finding. It never consults a model.
func ScopeFactsOf(f ScopeFinding, diffFiles []string, prior []ScopeFinding) ScopeFacts {
	facts := ScopeFacts{
		Critical:  strings.EqualFold(strings.TrimSpace(f.Severity), string(apiv1.SeverityCritical)),
		Unfixable: unfixableCategory(f.Category),
	}
	file := cleanScopePath(f.File)
	for _, d := range diffFiles {
		d = cleanScopePath(d)
		if d == "" {
			continue
		}
		if (file != "" && file == d) || strings.Contains(f.Text, d) {
			facts.NamesDiffFile = true
			break
		}
	}
	facts.OutsideDiff = file != "" && !facts.NamesDiffFile
	key := scopeKey(f)
	for _, p := range prior {
		if scopeKey(p) == key {
			facts.UnchangedFromPrior = true
			break
		}
	}
	return facts
}

func unfixableCategory(c string) bool {
	switch strings.ToLower(strings.TrimSpace(c)) {
	case string(apiv1.FindingRebaseNeeded), string(apiv1.FindingCrossPRBlocked):
		return true
	}
	return false
}

func cleanScopePath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if i := strings.LastIndex(p, ":"); i > 0 && isDigits(p[i+1:]) {
		p = p[:i]
	}
	if p == "" {
		return ""
	}
	return strings.TrimPrefix(path.Clean(p), "./")
}

func scopeKey(f ScopeFinding) string {
	return strings.ToLower(strings.TrimSpace(f.Category)) + "\x00" + cleanScopePath(f.File) + "\x00" + strings.Join(strings.Fields(strings.ToLower(f.Text)), " ")
}

// scopeDigest identifies a finding in records without storing its text.
func scopeDigest(f ScopeFinding) string {
	sum := sha256.Sum256([]byte(scopeKey(f)))
	return hex.EncodeToString(sum[:8])
}

// CodeScopeOutcome applies the code-level rules. decided is false when only
// the model can say. A critical finding, or one naming a diff file, is never a
// would-demote without code-level agreement; only an unfixable category
// supplies that agreement.
func CodeScopeOutcome(facts ScopeFacts) (outcome ScopeOutcome, decided bool) {
	switch {
	case facts.Critical:
		return ScopeInScope, true
	case facts.Unfixable:
		return ScopeWouldDemote, true
	case facts.NamesDiffFile:
		return ScopeInScope, true
	}
	return ScopeUnsure, false
}

// ScopeRecord is the shadow record for one finding. It holds a digest and
// numbers, never the finding text or the criteria.
type ScopeRecord struct {
	Digest      string       `json:"digest"`
	Severity    string       `json:"severity,omitempty"`
	Category    string       `json:"category,omitempty"`
	Facts       ScopeFacts   `json:"facts"`
	Source      ScopeSource  `json:"source"`
	Outcome     ScopeOutcome `json:"outcome"`
	Probability float64      `json:"probability,omitempty"`
	Cached      bool         `json:"cached,omitempty"`
	Error       string       `json:"error,omitempty"`
	// Reference is a hand label (in-scope or would-demote) joined offline.
	Reference ScopeOutcome `json:"reference,omitempty"`
}

var scopeQuestion = decider.Noul(
	"Given the acceptance criteria and the facts about the change, is this review finding about work that is in scope for this change?",
	&decider.NoulCriteria{
		True:  "The finding concerns the requested behavior or the changed files and is something the implementer should address in this change.",
		False: "The finding asks for work outside the acceptance criteria, about files or behavior this change does not touch, or that the implementer cannot resolve.",
	},
)

type scopeState struct {
	Criteria      string     `json:"criteria"`
	Finding       string     `json:"finding"`
	Category      string     `json:"category,omitempty"`
	File          string     `json:"file,omitempty"`
	DiffFiles     []string   `json:"diffFiles"`
	Facts         ScopeFacts `json:"facts"`
	DiffTruncated bool       `json:"diffTruncated,omitempty"`
}

// ScopeCheck judges every finding in the input. Code facts decide first; the
// model is asked only about the remainder, behind the scope threshold, and
// only a confident out-of-scope answer yields would-demote. Uncertain answers,
// model errors and timeouts keep the finding in the verdict (ScopeUnsure with
// the error recorded). It never returns an error: a failed check must not
// change a run.
func (g *Gate) ScopeCheck(ctx context.Context, in ScopeInput) []ScopeRecord {
	out := make([]ScopeRecord, 0, len(in.Findings))
	calls := 0
	for _, f := range in.Findings {
		rec := ScopeRecord{Digest: scopeDigest(f), Severity: f.Severity, Category: f.Category,
			Facts: ScopeFactsOf(f, in.DiffFiles, in.PriorFindings), Source: ScopeByCode, Outcome: ScopeUnsure}
		if o, ok := CodeScopeOutcome(rec.Facts); ok {
			rec.Outcome = o
		} else if calls < MaxScopeModelCalls {
			calls++
			g.judgeScope(ctx, f, in, &rec)
		}
		out = append(out, rec)
	}
	return out
}

func (g *Gate) judgeScope(ctx context.Context, f ScopeFinding, in ScopeInput, rec *ScopeRecord) {
	rec.Source = ScopeByModel
	files := in.DiffFiles
	st := scopeState{Criteria: clip(in.Criteria, maxScopeCriteria), Finding: clip(f.Text, maxScopeText),
		Category: f.Category, File: f.File, Facts: rec.Facts}
	if len(files) > maxScopeFiles {
		files, st.DiffTruncated = files[:maxScopeFiles], true
	}
	st.DiffFiles = files
	o, err := g.JudgeNoul(ctx, ScopeThresholdKey, st, scopeQuestion)
	rec.Probability, rec.Cached = o.Probability, o.Cached
	if err != nil {
		rec.Error = err.Error()
		return
	}
	switch o.Decision {
	case Yes:
		rec.Outcome = ScopeInScope
	case No:
		rec.Outcome = ScopeWouldDemote
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
