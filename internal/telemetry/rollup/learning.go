package rollup

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/learning"
)

// LearningEpisode is one finding-level durable projection of a correction or
// deterministic false-finding review/validation result.
type LearningEpisode struct {
	RunID              string                       `json:"runId"`
	SourceSeq          uint64                       `json:"sourceSeq"`
	FindingID          string                       `json:"findingId"`
	FindingIdentities  []string                     `json:"findingIdentities,omitempty"`
	Workflow           string                       `json:"workflow"`
	Stage              string                       `json:"stage,omitempty"`
	Gate               string                       `json:"gate"`
	SourceAttempt      int                          `json:"sourceAttempt"`
	NextAttempt        int                          `json:"nextAttempt,omitempty"`
	WorkflowDigest     string                       `json:"workflowDigest,omitempty"`
	GooberDigest       string                       `json:"gooberDigest,omitempty"`
	EffectiveVersion   string                       `json:"effectiveVersion,omitempty"`
	Signature          string                       `json:"signature"`
	Classification     apiv1.LearningClassification `json:"classification"`
	RecommendedAction  string                       `json:"recommendedAction"`
	Finding            apiv1.Finding                `json:"finding"`
	EvidenceJSON       string                       `json:"evidence"`
	CorrectionFeedback string                       `json:"correctionFeedback,omitempty"`
	Outcome            string                       `json:"outcome"`
	OccurredAt         time.Time                    `json:"occurredAt"`
}

// LearningEpisodeRequest limits a learning-episode query.
type LearningEpisodeRequest struct {
	Gaggle    string
	Workflow  string
	Signature string
	Since     time.Time
	Limit     int
}

// LearningCluster aggregates equivalent finding episodes while retaining
// exact run/sequence evidence.
type LearningCluster struct {
	Signature         string                       `json:"signature"`
	Classification    apiv1.LearningClassification `json:"classification"`
	Count             int                          `json:"count"`
	RunCount          int                          `json:"runCount"`
	Episodes          []JournalPointer             `json:"episodes"`
	RecommendedAction string                       `json:"recommendedAction"`
}

type learningClusterKey struct {
	signature         string
	classification    apiv1.LearningClassification
	recommendedAction string
}

func insertLearningEpisodes(ctx context.Context, tx *sql.Tx, id runIdentity, events []journalEvent) error {
	for i, event := range events {
		findings := eventLearningFindings(event)
		forcedOutcome := ""
		if !isLearningFailure(event) {
			findings = eventRunnerFindings(event, "disprovenLearningFindings")
			if len(findings) == 0 {
				continue
			}
			forcedOutcome = learning.OutcomeFalseFinding
		}
		if len(findings) == 0 {
			findings = []apiv1.Finding{syntheticLearningFinding(event)}
		}
		sourceAttempt := learningSourceAttempt(event)
		nextAttempt := learningNextAttempt(event, events[i+1:])
		correction := runnerString(event, "correctionFeedback")
		if correction == "" && event.Error != nil {
			correction = event.Error.Message
		}
		for _, finding := range findings {
			learning.NormalizeFinding(&finding, learningSubject(event), finding.EvidenceDigest)
			findingJSON, err := json.Marshal(finding)
			if err != nil {
				return fmt.Errorf("rollup: encode learning finding: %w", err)
			}
			evidence, err := json.Marshal(map[string]any{
				"runId": id.RunID, "seq": event.Seq, "stage": event.Stage,
				"gate": event.Gate, "verdict": event.Verdict, "status": event.Status,
				"sourceAttempt": sourceAttempt, "nextAttempt": nextAttempt,
				"artifacts": event.Artifacts, "verdictRef": event.Ref,
				"error": event.Error, "findingId": finding.ID,
			})
			if err != nil {
				return fmt.Errorf("rollup: encode learning evidence: %w", err)
			}
			outcome := forcedOutcome
			if outcome == "" {
				outcome = learningOutcome(event, finding.ID, finding.LearningSignature, events[i+1:])
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO learning_episodes
				(run_id, source_seq, finding_id, workflow, stage, gate,
				 source_attempt, next_attempt, workflow_digest, goober_digest,
				 effective_version, signature, classification, recommended_action,
				 finding_json, evidence_json, correction_feedback, outcome, occurred_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, NULLIF(?, 0), NULLIF(?, ''),
					NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?)`,
				id.RunID, event.Seq, finding.ID, id.Workflow, learningStage(event),
				learningSubject(event), sourceAttempt, nextAttempt,
				id.WorkflowDigest, id.GooberDigest,
				learning.EffectiveVersion(id.WorkflowDigest, id.GooberDigest),
				finding.LearningSignature, finding.LearningClassification,
				learning.RecommendedAction(finding.LearningClassification),
				string(findingJSON), string(evidence), correction, outcome,
				formatTime(event.Time).String)
			if err != nil {
				return fmt.Errorf("rollup: insert learning episode %s/%d/%s: %w", id.RunID, event.Seq, finding.ID, err)
			}
		}
	}
	return nil
}

func isLearningPass(event journalEvent) bool {
	if event.Type == eventStageFinished {
		return strings.EqualFold(event.Status, "success") || strings.EqualFold(event.Status, "no-work")
	}
	return strings.EqualFold(event.Verdict, "pass")
}

func isLearningFailure(event journalEvent) bool {
	if event.Type == eventGateEvaluated {
		return event.Gate != "" && !isLearningPass(event)
	}
	return event.Type == eventStageFinished && event.Stage != "" && !isLearningPass(event)
}

func isLearningResult(event journalEvent) bool {
	return event.Type == eventGateEvaluated || event.Type == eventStageFinished
}

func learningSubject(event journalEvent) string {
	if event.Type == eventStageFinished {
		return event.Stage
	}
	return event.Gate
}

func learningStage(event journalEvent) string {
	if event.Type == eventStageFinished {
		return event.Stage
	}
	return event.Target
}

func sameLearningSubject(first, second journalEvent) bool {
	return isLearningResult(second) && learningSubject(first) == learningSubject(second)
}

func learningSourceAttempt(event journalEvent) int {
	if event.Attempt > 0 {
		return event.Attempt
	}
	if attempt, ok := runnerNumber(event, "repassAttempt"); ok && attempt > 0 {
		return attempt
	}
	return 1
}

func learningNextAttempt(event journalEvent, later []journalEvent) int {
	target := event.Target
	if event.Type == eventStageFinished {
		target = event.Stage
	}
	for _, candidate := range later {
		if candidate.Type == eventStageStarted && candidate.Stage == target {
			return candidate.Attempt
		}
	}
	return 0
}

func learningOutcome(source journalEvent, findingID, signature string, later []journalEvent) string {
	for _, candidate := range later {
		if !sameLearningSubject(source, candidate) {
			continue
		}
		if runnerContains(candidate, "disprovenFindingIdentities", findingID) {
			return learning.OutcomeFalseFinding
		}
		if runnerContains(candidate, "resolvedFindingIdentities", findingID) ||
			runnerContains(candidate, "suppressedFindingIdentities", findingID) {
			return learning.OutcomeFixed
		}
		if isLearningPass(candidate) {
			if runnerString(candidate, "reason") == "REVIEW_FINDING_DISPROVEN" {
				return learning.OutcomeFalseFinding
			}
			return learning.OutcomeFixed
		}
		for _, finding := range eventLearningFindings(candidate) {
			if finding.ID == findingID || (finding.ID == "" && finding.LearningSignature == signature) {
				return learning.OutcomeRepeated
			}
		}
		return learning.OutcomeChangedFailure
	}
	for _, candidate := range later {
		if candidate.Type == eventRunFinished {
			switch strings.ToLower(candidate.Status) {
			case "escalated", "failed", "aborted":
				return learning.OutcomeEscalated
			default:
				return learning.OutcomeUnresolved
			}
		}
	}
	if source.Escalated {
		return learning.OutcomeEscalated
	}
	return learning.OutcomeUnresolved
}

func syntheticLearningFinding(event journalEvent) apiv1.Finding {
	message := runnerString(event, "correctionFeedback")
	if message == "" && event.Error != nil {
		message = event.Error.Message
		if message == "" {
			message = event.Error.Code
		}
	}
	if message == "" {
		message = learningSubject(event) + " returned " + learningResultValue(event)
	}
	finding := apiv1.Finding{
		Severity:               apiv1.SeverityError,
		Message:                message,
		Location:               learningStage(event),
		LearningClassification: apiv1.LearningValidation,
	}
	learning.NormalizeFinding(&finding, learningSubject(event), runnerString(event, "diffDigest"))
	return finding
}

func learningResultValue(event journalEvent) string {
	if event.Type == eventStageFinished {
		return event.Status
	}
	return event.Verdict
}

func eventLearningFindings(event journalEvent) []apiv1.Finding {
	if event.Runner == nil {
		return nil
	}
	out := eventRunnerFindings(event, "learningFindings")
	if len(out) > 0 {
		return out
	}

	// Compatibility for journals written by the initial projection, before
	// gate events carried complete finding records.
	identities, ok := event.Runner["findingIdentities"].([]any)
	if !ok {
		return nil
	}
	classification := apiv1.LearningInstruction
	if event.Type == eventStageFinished {
		classification = apiv1.LearningValidation
	}
	signature := strings.Join([]string{
		learningSubject(event), learningResultValue(event), runnerString(event, "failureSignature"),
	}, "|")
	message := runnerString(event, "correctionFeedback")
	if message == "" {
		message = learningSubject(event) + " returned " + learningResultValue(event)
	}
	out = make([]apiv1.Finding, 0, len(identities))
	for _, value := range identities {
		id, _ := value.(string)
		if id == "" {
			continue
		}
		out = append(out, apiv1.Finding{
			ID: id, Message: message, Location: learningStage(event),
			LearningSignature: signature, LearningClassification: classification,
		})
	}
	return out
}

func eventRunnerFindings(event journalEvent, key string) []apiv1.Finding {
	if event.Runner == nil {
		return nil
	}
	raw, ok := event.Runner[key].([]any)
	if ok {
		out := make([]apiv1.Finding, 0, len(raw))
		for _, value := range raw {
			data, err := json.Marshal(value)
			if err != nil {
				continue
			}
			var finding apiv1.Finding
			if json.Unmarshal(data, &finding) == nil && finding.Message != "" {
				out = append(out, finding)
			}
		}
		return out
	}
	if typed, ok := event.Runner[key].([]apiv1.Finding); ok {
		return append([]apiv1.Finding(nil), typed...)
	}
	return nil
}

func runnerString(event journalEvent, key string) string {
	if event.Runner == nil {
		return ""
	}
	value, _ := event.Runner[key].(string)
	return value
}

func runnerNumber(event journalEvent, key string) (int, bool) {
	if event.Runner == nil {
		return 0, false
	}
	switch value := event.Runner[key].(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	case json.Number:
		n, err := value.Int64()
		return int(n), err == nil
	}
	return 0, false
}

func runnerContains(event journalEvent, key, want string) bool {
	if event.Runner == nil || want == "" {
		return false
	}
	switch values := event.Runner[key].(type) {
	case []any:
		for _, value := range values {
			if value == want {
				return true
			}
		}
	case []string:
		for _, value := range values {
			if value == want {
				return true
			}
		}
	}
	return false
}

// LearningEpisodes returns durable correction outcomes, ordered by occurrence.
func (db *DB) LearningEpisodes(ctx context.Context, req LearningEpisodeRequest) ([]LearningEpisode, error) {
	clauses := []string{"1=1"}
	args := []any{}
	if req.Gaggle != "" {
		clauses = append(clauses, "r.gaggle = ?")
		args = append(args, req.Gaggle)
	}
	if req.Workflow != "" {
		clauses = append(clauses, "e.workflow = ?")
		args = append(args, req.Workflow)
	}
	if req.Signature != "" {
		clauses = append(clauses, "e.signature = ?")
		args = append(args, req.Signature)
	}
	if !req.Since.IsZero() {
		clauses = append(clauses, "e.occurred_at >= ?")
		args = append(args, formatTime(req.Since).String)
	}
	query := `SELECT e.run_id, e.source_seq, e.finding_id, e.workflow, e.stage,
		e.gate, e.source_attempt, COALESCE(e.next_attempt, 0),
		COALESCE(e.workflow_digest, ''), COALESCE(e.goober_digest, ''),
		COALESCE(e.effective_version, ''), e.signature, e.classification,
		e.recommended_action, e.finding_json, e.evidence_json,
		COALESCE(e.correction_feedback, ''), e.outcome, e.occurred_at
		FROM learning_episodes e JOIN runs r ON r.run_id = e.run_id
		WHERE ` + strings.Join(clauses, " AND ") + `
		ORDER BY e.occurred_at, e.run_id, e.source_seq, e.finding_id`
	if req.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, req.Limit)
	}
	rows, err := db.readDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query learning episodes: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LearningEpisode
	for rows.Next() {
		var episode LearningEpisode
		var classification, findingJSON, at string
		if err := rows.Scan(
			&episode.RunID, &episode.SourceSeq, &episode.FindingID,
			&episode.Workflow, &episode.Stage, &episode.Gate,
			&episode.SourceAttempt, &episode.NextAttempt,
			&episode.WorkflowDigest, &episode.GooberDigest,
			&episode.EffectiveVersion, &episode.Signature, &classification,
			&episode.RecommendedAction, &findingJSON, &episode.EvidenceJSON,
			&episode.CorrectionFeedback, &episode.Outcome, &at,
		); err != nil {
			return nil, fmt.Errorf("rollup: scan learning episode: %w", err)
		}
		episode.Classification = apiv1.LearningClassification(classification)
		if err := json.Unmarshal([]byte(findingJSON), &episode.Finding); err != nil {
			return nil, fmt.Errorf("rollup: decode learning finding: %w", err)
		}
		episode.FindingIdentities = []string{episode.FindingID}
		episode.OccurredAt, err = parseTime(sql.NullString{String: at, Valid: at != ""})
		if err != nil {
			return nil, err
		}
		out = append(out, episode)
	}
	return out, rows.Err()
}

// LearningClusters groups episodes by normalized signature for Tutor and work
// nomination analysis.
func (db *DB) LearningClusters(ctx context.Context, req LearningEpisodeRequest) ([]LearningCluster, error) {
	episodes, err := db.LearningEpisodes(ctx, req)
	if err != nil {
		return nil, err
	}
	grouped := map[learningClusterKey]*LearningCluster{}
	runs := map[learningClusterKey]map[string]bool{}
	for _, episode := range episodes {
		if episode.Outcome == learning.OutcomeFalseFinding || episode.Outcome == learning.OutcomeUnresolved {
			continue
		}
		key := learningClusterKey{
			signature:         episode.Signature,
			classification:    episode.Classification,
			recommendedAction: episode.RecommendedAction,
		}
		cluster := grouped[key]
		if cluster == nil {
			cluster = &LearningCluster{
				Signature: episode.Signature, Classification: episode.Classification,
				RecommendedAction: episode.RecommendedAction,
			}
			grouped[key] = cluster
			runs[key] = map[string]bool{}
		}
		cluster.Count++
		cluster.Episodes = append(cluster.Episodes, JournalPointer{RunID: episode.RunID, Seq: episode.SourceSeq})
		runs[key][episode.RunID] = true
	}
	out := make([]LearningCluster, 0, len(grouped))
	for key, cluster := range grouped {
		cluster.RunCount = len(runs[key])
		out = append(out, *cluster)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RunCount != out[j].RunCount {
			return out[i].RunCount > out[j].RunCount
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].Signature != out[j].Signature {
			return out[i].Signature < out[j].Signature
		}
		if out[i].Classification != out[j].Classification {
			return out[i].Classification < out[j].Classification
		}
		return out[i].RecommendedAction < out[j].RecommendedAction
	})
	return out, nil
}
