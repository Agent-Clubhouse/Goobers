// Package remediation provides the read-only institutional-memory index used by
// remediation agents. It deliberately has no journal or provider write path.
package remediation

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/api/integrity"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readmodel"
)

const (
	embeddingDimensions = 128
	excerptLimit        = 2000
)

var tokenPattern = regexp.MustCompile(`[[:alnum:]_./:-]+`)

// Record is an outcome-verified remediation remembered from a prior run.
// Excerpts must already be bounded; NewIndex scrubs and bounds them again
// before indexing or returning them.
type Record struct {
	ID             string
	Stage          string
	ErrorClass     string
	FailureExcerpt string
	FixExcerpt     string
	DidItHelp      bool
	OutcomeKnown   bool
	ObservedAt     time.Time
	ConfigDigest   string
	Integrity      integrity.Grade
}

// Query describes the failure for which examples are requested.
type Query struct {
	Stage          string
	ErrorClass     string
	FailureExcerpt string
	ConfigDigest   string
}

// Options controls a bounded retrieval. Now is injectable for deterministic
// callers and tests. K <= 0 uses DefaultK.
type Options struct {
	K   int
	Now time.Time
}

// DefaultK is the number of historical examples returned when no limit is set.
const DefaultK = 3

// Result is a successful, redacted few-shot example. DidItHelp is intentionally
// repeated on the result so consumers cannot mistake similarity for evidence.
type Result struct {
	Record
	Score   float64
	FewShot string
}

// Index is an immutable, in-memory read model of verified remediations.
type Index struct {
	records []indexedRecord
	scrub   journal.Scrubber
}

// LoadIndex builds the read-only corpus from read.db's projected remediation
// examples. The loader is read-only and consumes projection output only.
func LoadIndex(runsDir string, scrubber journal.Scrubber) (*Index, error) {
	readDBPath := readModelPathFromRunsDir(runsDir)
	if _, err := os.Stat(readDBPath); err != nil {
		return nil, err
	}
	store, err := readmodel.Open(readDBPath)
	if err != nil {
		return nil, fmt.Errorf("remediation: open read model: %w", err)
	}
	defer func() { _ = store.Close() }()

	rows, err := store.RemediationExamples(context.Background(), 0)
	if err != nil {
		return nil, fmt.Errorf("remediation: list remediation examples: %w", err)
	}
	records := make([]Record, 0, len(rows))
	for _, row := range rows {
		records = append(records, Record{
			ID:             fmt.Sprintf("%s:%s:%d", row.RunID, row.Stage, row.Attempt),
			Stage:          row.Stage,
			ErrorClass:     row.ErrorClass,
			FailureExcerpt: row.FailureExcerpt,
			FixExcerpt:     row.FixExcerpt,
			DidItHelp:      row.DidItHelp,
			OutcomeKnown:   true,
			ObservedAt:     row.ObservedAt,
			ConfigDigest:   row.ConfigDigest,
			Integrity:      integrity.Trusted,
		})
	}
	return NewIndex(records, scrubber), nil
}

func readModelPathFromRunsDir(runsDir string) string {
	parent := filepath.Dir(runsDir)
	if filepath.Base(filepath.Dir(parent)) == "gaggles" {
		return filepath.Join(filepath.Dir(filepath.Dir(parent)), readmodel.FileName)
	}
	return filepath.Join(parent, readmodel.FileName)
}

// AugmentInvocation adds retrieved examples to an agent's instructions. It is
// deliberately an addendum: the caller still owns the workflow and gate
// decision, and no historical fix is applied automatically.
func AugmentInvocation(env apiv1.InvocationEnvelope, index *Index, query Query, options Options) apiv1.InvocationEnvelope {
	if index == nil {
		return env
	}
	results := index.Retrieve(query, options)
	if len(results) == 0 {
		return env
	}
	var examples strings.Builder
	for _, result := range results {
		if examples.Len() > 0 {
			examples.WriteString("\n\n")
		}
		examples.WriteString(result.FewShot)
	}
	const heading = "Outcome-verified historical remediation examples (guidance only; do not apply blindly):\n"
	if strings.TrimSpace(env.InstructionAddendum) == "" {
		env.InstructionAddendum = heading + examples.String()
	} else {
		env.InstructionAddendum += "\n\n" + heading + examples.String()
	}
	return env
}

type indexedRecord struct {
	record    Record
	embedding [embeddingDimensions]float64
}

// NewIndex builds a read-only index. Records that are not outcome-verified,
// lack an identity, or have unapproved content are excluded fail-closed.
func NewIndex(records []Record, scrubber journal.Scrubber) *Index {
	if scrubber == nil {
		scrubber = journal.NewPatternScrubber()
	}
	out := &Index{scrub: scrubber}
	for _, input := range records {
		if !input.OutcomeKnown || input.ID == "" || input.Stage == "" ||
			input.Integrity == "" || input.Integrity == integrity.Unapproved {
			continue
		}
		record := input
		record.FailureExcerpt = cleanExcerpt(scrubber, input.FailureExcerpt)
		record.FixExcerpt = cleanExcerpt(scrubber, input.FixExcerpt)
		if record.FailureExcerpt == "" || record.FixExcerpt == "" {
			continue
		}
		record.Stage = cleanMetadata(scrubber, record.Stage)
		record.ErrorClass = cleanMetadata(scrubber, record.ErrorClass)
		record.ConfigDigest = cleanMetadata(scrubber, record.ConfigDigest)
		out.records = append(out.records, indexedRecord{
			record:    record,
			embedding: embed(signature(record.Stage, record.ErrorClass, record.FailureExcerpt)),
		})
	}
	return out
}

// Retrieve returns at most K examples, ranked by semantic similarity, verified
// outcome, and freshness. It never mutates the corpus or applies a fix.
func (i *Index) Retrieve(query Query, options Options) []Result {
	if i == nil || len(i.records) == 0 {
		return nil
	}
	k := options.K
	if k <= 0 {
		k = DefaultK
	}
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	q := embed(signature(
		cleanMetadata(i.scrub, query.Stage),
		cleanMetadata(i.scrub, query.ErrorClass),
		cleanExcerpt(i.scrub, query.FailureExcerpt),
	))
	results := make([]Result, 0, len(i.records))
	for _, item := range i.records {
		similarity := cosine(q, item.embedding)
		if similarity <= 0 {
			continue
		}
		score := similarity * freshness(item.record, query.ConfigDigest, now) * outcomeWeight(item.record.DidItHelp)
		results = append(results, Result{
			Record:  item.record,
			Score:   score,
			FewShot: fewShot(item.record),
		})
	}
	sort.SliceStable(results, func(a, b int) bool {
		if results[a].Score != results[b].Score {
			return results[a].Score > results[b].Score
		}
		return results[a].ID < results[b].ID
	})
	if len(results) > k {
		results = results[:k]
	}
	return results
}

func cleanExcerpt(scrubber journal.Scrubber, value string) string {
	value = string(scrubber.Scrub([]byte(value)))
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > excerptLimit {
		runes = runes[:excerptLimit]
	}
	return string(runes)
}

func cleanMetadata(scrubber journal.Scrubber, value string) string {
	return strings.Join(strings.Fields(string(scrubber.Scrub([]byte(value)))), " ")
}

func signature(stage, class, excerpt string) string {
	return strings.Join([]string{stage, class, excerpt}, " ")
}

func embed(text string) [embeddingDimensions]float64 {
	var vector [embeddingDimensions]float64
	for _, token := range tokenPattern.FindAllString(strings.ToLower(text), -1) {
		digest := sha256.Sum256([]byte(token))
		index := (int(digest[0])<<8 | int(digest[1])) % embeddingDimensions
		sign := 1.0
		if digest[2]&1 != 0 {
			sign = -1
		}
		vector[index] += sign
	}
	return vector
}

func cosine(a, b [embeddingDimensions]float64) float64 {
	var dot, aa, bb float64
	for index := range a {
		dot += a[index] * b[index]
		aa += a[index] * a[index]
		bb += b[index] * b[index]
	}
	if aa == 0 || bb == 0 {
		return 0
	}
	return dot / math.Sqrt(aa*bb)
}

func freshness(record Record, configDigest string, now time.Time) float64 {
	weight := 1.0
	if configDigest != "" && record.ConfigDigest != "" && configDigest != record.ConfigDigest {
		weight *= 0.5
	}
	if !record.ObservedAt.IsZero() && now.After(record.ObservedAt) {
		age := now.Sub(record.ObservedAt).Hours() / (24 * 90)
		weight *= math.Pow(0.5, age)
	}
	return weight
}

func outcomeWeight(didItHelp bool) float64 {
	if didItHelp {
		return 1
	}
	return 0.25
}

func fewShot(record Record) string {
	return fmt.Sprintf("Past failure (%s/%s), did-it-help: %t\nFailure: %s\nVerified fix: %s",
		record.Stage, record.ErrorClass, record.DidItHelp, record.FailureExcerpt, record.FixExcerpt)
}
