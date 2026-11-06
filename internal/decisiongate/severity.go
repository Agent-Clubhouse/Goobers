package decisiongate

import (
	"context"
	"regexp"
	"strings"

	"github.com/goobers/goobers/internal/decider"
)

// SeverityThresholdKey is the threshold key for the review-comment blocking
// question.
const SeverityThresholdKey = "review_comment_blocking"

// DefaultSeverityThreshold keeps ambiguous comments on the existing path.
var DefaultSeverityThreshold = Threshold{Accept: 0.9, Reject: 0.1}

// Severity classifies one review comment for remediation.
type Severity string

// Severities. SeverityUnsure means the caller keeps today's behavior and
// treats the comment as actionable.
const (
	SeverityBlocking Severity = "blocking"
	SeverityNit      Severity = "nit"
	SeverityQuestion Severity = "question"
	SeverityUnsure   Severity = "unsure"
)

var (
	severityLabel = regexp.MustCompile(`(?i)^\s*(?:\*\*)?(nit|nitpick|question|blocking|blocker)(?:\*\*)?\s*[:\-(\[]`)
	// maxSeverityComment bounds what is sent to the model.
	maxSeverityComment = 4000
)

var severityPrompt = decider.Noul(
	"Does this code review comment identify a defect, regression, missing requirement or risk that must be fixed before the change can merge?",
	&decider.NoulCriteria{
		True:  "The comment asks for a change that is required for correctness, safety or the stated requirements.",
		False: "The comment is a style preference, optional suggestion, praise, or a clarifying question.",
	},
)

// SeverityFromText returns a severity only when the comment states one
// itself: an explicit label prefix, or a short comment that is only a
// question. It is a code-decided fact, so no model is consulted for it.
func SeverityFromText(comment string) (Severity, bool) {
	if m := severityLabel.FindStringSubmatch(comment); m != nil {
		switch strings.ToLower(m[1]) {
		case "nit", "nitpick":
			return SeverityNit, true
		case "question":
			return SeverityQuestion, true
		default:
			return SeverityBlocking, true
		}
	}
	trimmed := strings.TrimSpace(comment)
	if trimmed != "" && strings.HasSuffix(trimmed, "?") && !strings.Contains(trimmed, "\n") && len(trimmed) <= 200 {
		return SeverityQuestion, true
	}
	return SeverityUnsure, false
}

// EvaluateSeverity classifies a review comment. Stated labels and empty text
// are decided in code; otherwise the model judges only whether the comment
// must be fixed, and a non-blocking answer is reported as a nit. Uncertain
// answers and errors return SeverityUnsure so the caller's existing path runs.
func (g *Gate) EvaluateSeverity(ctx context.Context, comment string) (Severity, Outcome, error) {
	if sev, ok := SeverityFromText(comment); ok {
		return sev, Outcome{Name: SeverityThresholdKey}, nil
	}
	if strings.TrimSpace(comment) == "" {
		return SeverityUnsure, Outcome{Name: SeverityThresholdKey}, nil
	}
	if len(comment) > maxSeverityComment {
		comment = comment[:maxSeverityComment]
	}
	o, err := g.JudgeNoul(ctx, SeverityThresholdKey, comment, severityPrompt)
	if err != nil {
		return SeverityUnsure, o, err
	}
	switch o.Decision {
	case Yes:
		return SeverityBlocking, o, nil
	case No:
		return SeverityNit, o, nil
	}
	return SeverityUnsure, o, nil
}

// SeverityRecord pairs the model's label with a reference label (human or
// reviewer agent) so agreement can be measured offline. It holds no comment
// text.
type SeverityRecord struct {
	Predicted Severity
	Reference Severity
	// Coded is true when code, not the model, decided Predicted.
	Coded bool
	Err   error
}

// SeverityTally summarizes shadow agreement on the comments the model judged.
// Coded decisions are counted separately because they say nothing about the
// model.
type SeverityTally struct {
	Total, Coded, Unsure, Errors int
	Judged, Agree                int
	// MissedBlocking counts blocking references the model called a nit: the
	// costly disagreement.
	MissedBlocking int
}

// Add folds one record in. Records without a reference label are skipped for
// agreement. Reference nits and questions both count as non-blocking.
func (t *SeverityTally) Add(r SeverityRecord) {
	t.Total++
	if r.Err != nil {
		t.Errors++
	}
	if r.Coded {
		t.Coded++
		return
	}
	if r.Predicted == SeverityUnsure {
		t.Unsure++
		return
	}
	if r.Reference == "" {
		return
	}
	t.Judged++
	predBlocking := r.Predicted == SeverityBlocking
	refBlocking := r.Reference == SeverityBlocking
	if predBlocking == refBlocking {
		t.Agree++
	} else if refBlocking {
		t.MissedBlocking++
	}
}

// Agreement is the share of judged comments where the model matched the
// reference. It refuses a figure below minSample judged comments.
func (t SeverityTally) Agreement(minSample int) (float64, bool) {
	if t.Judged == 0 || t.Judged < minSample {
		return 0, false
	}
	return float64(t.Agree) / float64(t.Judged), true
}

// ShadowSeverity scores a comment without influencing anything. Errors are
// recorded and swallowed: a shadow failure must never change a run.
func (g *Gate) ShadowSeverity(ctx context.Context, comment string, reference Severity) SeverityRecord {
	_, coded := SeverityFromText(comment)
	sev, _, err := g.EvaluateSeverity(ctx, comment)
	return SeverityRecord{Predicted: sev, Reference: reference, Coded: coded, Err: err}
}
