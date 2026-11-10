package decisiongate

import "sort"

// Ledger entry kinds. A prediction is written when a failed run is triaged; an
// outcome is written later, once the failure's eventual cause is known.
const (
	CIEntryPrediction = "prediction"
	CIEntryOutcome    = "outcome"
)

// CIFailureEntry is one line of the triage ledger. It holds digests and
// numbers, never log text, so the ledger is safe to keep and share.
type CIFailureEntry struct {
	Kind   string `json:"kind"`
	RunKey string `json:"runKey"`
	// Class is the predicted class on a prediction and the eventual class on
	// an outcome.
	Class       CIFailureClass  `json:"class"`
	Source      CIFailureSource `json:"source,omitempty"`
	Probability float64         `json:"probability,omitempty"`
	Digest      string          `json:"digest,omitempty"`
}

// ValidEventualCIClass reports whether c can be an eventual outcome: the model
// is scored against a concrete cause, never against "unsure".
func ValidEventualCIClass(c CIFailureClass) bool {
	switch c {
	case CIFlaky, CIReal, CIInfra, CIEnvironment:
		return true
	}
	return false
}

// CIConfusion counts resolved predictions by predicted then eventual class.
type CIConfusion map[CIFailureClass]map[CIFailureClass]int

func (c CIConfusion) add(predicted, eventual CIFailureClass) {
	if c[predicted] == nil {
		c[predicted] = map[CIFailureClass]int{}
	}
	c[predicted][eventual]++
}

// CIMatrix is the shadow confusion matrix: each prediction joined to the
// eventual outcome of the same run. Model and code decisions are kept apart
// because code facts say nothing about the model.
type CIMatrix struct {
	Model CIConfusion `json:"model"`
	Code  CIConfusion `json:"code"`
	// Pending predictions have no outcome yet. Orphans are outcomes with no
	// prediction. Both are excluded from every score.
	Pending int `json:"pending"`
	Orphans int `json:"orphans"`
	// Unsure counts resolved predictions where nothing was decided; the
	// implementer got the failure, exactly as today.
	Unsure int `json:"unsure"`
	// WronglySkipped counts model calls of flaky, infra or environment whose
	// eventual cause was real: the costly miss, where a real defect would have
	// bypassed the implementer.
	WronglySkipped int `json:"wronglySkipped"`
}

// JoinCIFailures builds the confusion matrix from ledger entries. The latest
// prediction and the latest outcome per run key win, so a re-triage or a
// corrected outcome replaces the earlier line.
func JoinCIFailures(entries []CIFailureEntry) CIMatrix {
	preds := map[string]CIFailureEntry{}
	outs := map[string]CIFailureClass{}
	for _, e := range entries {
		if e.RunKey == "" {
			continue
		}
		switch e.Kind {
		case CIEntryPrediction:
			preds[e.RunKey] = e
		case CIEntryOutcome:
			if ValidEventualCIClass(e.Class) {
				outs[e.RunKey] = e.Class
			}
		}
	}
	m := CIMatrix{Model: CIConfusion{}, Code: CIConfusion{}}
	keys := make([]string, 0, len(preds))
	for k := range preds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := preds[k]
		eventual, ok := outs[k]
		if !ok {
			m.Pending++
			continue
		}
		switch {
		case p.Source == CISourceCode:
			m.Code.add(p.Class, eventual)
		case p.Class == CIUnsure || !ValidEventualCIClass(p.Class):
			m.Unsure++
		default:
			m.Model.add(p.Class, eventual)
			if p.Class != CIReal && eventual == CIReal {
				m.WronglySkipped++
			}
		}
	}
	for k := range outs {
		if _, ok := preds[k]; !ok {
			m.Orphans++
		}
	}
	return m
}

// ModelJudged is how many resolved predictions the model decided.
func (m CIMatrix) ModelJudged() int { return confusionTotal(m.Model) }

// ModelAccuracy is the share of model-decided, resolved predictions whose class
// equals the eventual one. It refuses a figure below minSample, so no rate is
// reported or acted on from a handful of runs.
func (m CIMatrix) ModelAccuracy(minSample int) (float64, bool) {
	n := m.ModelJudged()
	if n == 0 || n < minSample {
		return 0, false
	}
	agree := 0
	for class, row := range m.Model {
		agree += row[class]
	}
	return float64(agree) / float64(n), true
}

func confusionTotal(c CIConfusion) int {
	n := 0
	for _, row := range c {
		for _, v := range row {
			n += v
		}
	}
	return n
}
