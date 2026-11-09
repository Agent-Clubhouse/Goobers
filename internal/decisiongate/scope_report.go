package decisiongate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// DefaultScopeMinSample is the smallest denominator for which a scope report
// prints a percentage.
const DefaultScopeMinSample = 30

// WriteScopeRecords appends records to w as JSON lines.
func WriteScopeRecords(w io.Writer, recs []ScopeRecord) error {
	enc := json.NewEncoder(w)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// ReadScopeRecords reads JSON lines written by WriteScopeRecords. Blank lines
// are skipped.
func ReadScopeRecords(r io.Reader) ([]ScopeRecord, error) {
	var out []ScopeRecord
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for line := 1; sc.Scan(); line++ {
		if strings.TrimSpace(sc.Text()) == "" {
			continue
		}
		var rec ScopeRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			return nil, fmt.Errorf("scope record line %d: %w", line, err)
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// ScopeTally summarizes scope records.
type ScopeTally struct {
	Total, InScope, WouldDemote, Unsure, Errors int
	ByCode, ByModel                             int
	// Hand-labeled agreement over model-judged findings that carry a
	// reference and a decided outcome.
	Labeled, Agree int
	// WouldDemoteInScope counts model would-demotes whose reference says the
	// finding was in scope: the costly disagreement.
	WouldDemoteInScope int
	// DemotedCritical counts would-demotes of critical findings. It must stay
	// zero; the check never demotes them.
	DemotedCritical int
}

// Add folds one record in.
func (t *ScopeTally) Add(r ScopeRecord) {
	t.Total++
	if r.Error != "" {
		t.Errors++
	}
	switch r.Outcome {
	case ScopeInScope:
		t.InScope++
	case ScopeWouldDemote:
		t.WouldDemote++
		if r.Facts.Critical {
			t.DemotedCritical++
		}
	default:
		t.Unsure++
	}
	if r.Source == ScopeByCode {
		t.ByCode++
		return
	}
	t.ByModel++
	if r.Reference == "" || r.Outcome == ScopeUnsure {
		return
	}
	t.Labeled++
	if r.Outcome == r.Reference {
		t.Agree++
	} else if r.Outcome == ScopeWouldDemote {
		t.WouldDemoteInScope++
	}
}

// NewScopeTally tallies records.
func NewScopeTally(recs []ScopeRecord) ScopeTally {
	var t ScopeTally
	for _, r := range recs {
		t.Add(r)
	}
	return t
}

func scopePct(num, den, minSample int) string {
	if den < minSample {
		return fmt.Sprintf("n/a (n=%d<%d)", den, minSample)
	}
	return fmt.Sprintf("%.1f%% (%d/%d)", 100*float64(num)/float64(den), num, den)
}

// Render prints counts always and percentages only at or above minSample
// (zero means DefaultScopeMinSample).
func (t ScopeTally) Render(minSample int) string {
	if minSample <= 0 {
		minSample = DefaultScopeMinSample
	}
	var b strings.Builder
	fmt.Fprintf(&b, "findings=%d (code=%d model=%d) errors=%d\n", t.Total, t.ByCode, t.ByModel, t.Errors)
	fmt.Fprintf(&b, "in-scope=%d would-demote=%d unsure=%d\n", t.InScope, t.WouldDemote, t.Unsure)
	fmt.Fprintf(&b, "in-scope rate=%s\n", scopePct(t.InScope, t.Total, minSample))
	fmt.Fprintf(&b, "would-demote rate=%s\n", scopePct(t.WouldDemote, t.Total, minSample))
	fmt.Fprintf(&b, "unsure rate=%s\n", scopePct(t.Unsure, t.Total, minSample))
	fmt.Fprintf(&b, "hand-labeled model agreement=%s\n", scopePct(t.Agree, t.Labeled, minSample))
	fmt.Fprintf(&b, "would-demote of in-scope reference=%d critical-would-demote=%d\n", t.WouldDemoteInScope, t.DemotedCritical)
	fmt.Fprintf(&b, "enforce: not supported; shadow only\n")
	return b.String()
}
