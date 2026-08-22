package rollup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// LearningEpisode is the durable projection of one non-pass review or
// validation result. EvidenceJSON contains bounded journal pointers rather
// than copied transcript content.
type LearningEpisode struct {
	RunID              string    `json:"runId"`
	SourceSeq          uint64    `json:"sourceSeq"`
	Workflow           string    `json:"workflow"`
	Stage              string    `json:"stage,omitempty"`
	Gate               string    `json:"gate"`
	SourceAttempt      int       `json:"sourceAttempt"`
	NextAttempt        int       `json:"nextAttempt,omitempty"`
	WorkflowDigest     string    `json:"workflowDigest,omitempty"`
	GooberDigest       string    `json:"gooberDigest,omitempty"`
	EffectiveVersion   string    `json:"effectiveVersion,omitempty"`
	Signature          string    `json:"signature"`
	Classification     string    `json:"classification"`
	EvidenceJSON       string    `json:"evidence"`
	CorrectionFeedback string    `json:"correctionFeedback,omitempty"`
	Outcome            string    `json:"outcome"`
	OccurredAt         time.Time `json:"occurredAt"`
}

func recommendedLearningAction(classification string) string {
	switch strings.ToLower(classification) {
	case "validation":
		return "targeted-test-mapping"
	case "code", "code-defect":
		return "code-issue"
	case "workflow", "gate":
		return "workflow-or-gate"
	default:
		return "instruction-or-skill"
	}
}

// LearningEpisodeRequest limits a learning-episode query.
type LearningEpisodeRequest struct {
	Gaggle    string
	Workflow  string
	Signature string
	Since     time.Time
	Limit     int
}

// LearningCluster aggregates equivalent episodes while retaining every source
// run and sequence as evidence.
type LearningCluster struct {
	Signature         string           `json:"signature"`
	Classification    string           `json:"classification"`
	Count             int              `json:"count"`
	Episodes          []JournalPointer `json:"episodes"`
	RecommendedAction string           `json:"recommendedAction"`
}

func normalizeLearningSignature(gate, verdict, code string) string {
	parts := []string{strings.ToLower(strings.TrimSpace(gate)), strings.ToLower(strings.TrimSpace(verdict)), strings.ToLower(strings.TrimSpace(code))}
	return strings.Join(parts, "|")
}

func learningClassification(event journalEvent) string {
	if event.Runner != nil {
		if value, ok := event.Runner["classification"].(string); ok && strings.TrimSpace(value) != "" {
			return strings.ToLower(strings.TrimSpace(value))
		}
	}
	gate := strings.ToLower(event.Gate)
	if strings.Contains(gate, "review") {
		return "review"
	}
	return "validation"
}

func learningEffectiveVersion(id runIdentity) string {
	if id.WorkflowDigest == "" && id.GooberDigest == "" {
		return ""
	}
	b, _ := json.Marshal([]string{id.WorkflowDigest, id.GooberDigest})
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func insertLearningEpisodes(ctx context.Context, tx *sql.Tx, id runIdentity, events []journalEvent) error {
	for i, event := range events {
		if event.Type != eventGateEvaluated || strings.EqualFold(event.Verdict, "pass") || event.Gate == "" {
			continue
		}
		attempt := event.Attempt
		if attempt == 0 {
			attempt = 1
		}
		nextAttempt := 0
		for _, later := range events[i+1:] {
			if later.Type == eventStageStarted && later.Stage == event.Target {
				nextAttempt = later.Attempt
				break
			}
		}
		code := ""
		feedback := ""
		if event.Runner != nil {
			if value, ok := event.Runner["failureSignature"].(string); ok {
				code = value
			}
			if value, ok := event.Runner["correctionFeedback"].(string); ok {
				feedback = value
			}
		}
		signature := normalizeLearningSignature(event.Gate, event.Verdict, code)
		evidence, err := json.Marshal(map[string]any{
			"runId": id.RunID, "seq": event.Seq, "gate": event.Gate,
			"verdict": event.Verdict, "sourceAttempt": attempt,
			"nextAttempt": nextAttempt, "artifacts": event.Artifacts,
		})
		if err != nil {
			return fmt.Errorf("rollup: encode learning evidence: %w", err)
		}
		outcome := "escalated"
		for _, later := range events[i+1:] {
			if later.Type != eventGateEvaluated || later.Gate != event.Gate {
				continue
			}
			if strings.EqualFold(later.Verdict, "pass") {
				outcome = "fixed"
			} else if normalizeLearningSignature(later.Gate, later.Verdict, runnerSignature(later)) == signature {
				outcome = "repeated"
			} else {
				outcome = "changed-failure"
			}
			break
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO learning_episodes
			(run_id, source_seq, workflow, stage, gate, source_attempt, next_attempt,
			 workflow_digest, goober_digest, effective_version, signature,
			 classification, evidence_json, correction_feedback, outcome, occurred_at)
			VALUES (?, ?, ?, ?, ?, ?, NULLIF(?, 0), NULLIF(?, ''), NULLIF(?, ''),
				NULLIF(?, ''), ?, ?, ?, NULLIF(?, ''), ?, ?)`,
			id.RunID, event.Seq, id.Workflow, event.Target, event.Gate, attempt,
			nextAttempt, id.WorkflowDigest, id.GooberDigest, learningEffectiveVersion(id),
			signature, learningClassification(event), string(evidence), feedback,
			outcome, formatTime(event.Time))
		if err != nil {
			return fmt.Errorf("rollup: insert learning episode %s/%d: %w", id.RunID, event.Seq, err)
		}
	}
	return nil
}

func runnerSignature(event journalEvent) string {
	if event.Runner != nil {
		if value, ok := event.Runner["failureSignature"].(string); ok {
			return value
		}
	}
	return ""
}

// LearningEpisodes returns durable non-pass outcomes, ordered by occurrence.
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
	query := `SELECT e.run_id, e.source_seq, e.workflow, e.stage, e.gate,
		e.source_attempt, COALESCE(e.next_attempt, 0), COALESCE(e.workflow_digest, ''),
		COALESCE(e.goober_digest, ''), COALESCE(e.effective_version, ''),
		e.signature, e.classification, e.evidence_json,
		COALESCE(e.correction_feedback, ''), e.outcome, e.occurred_at
		FROM learning_episodes e JOIN runs r ON r.run_id = e.run_id
		WHERE ` + strings.Join(clauses, " AND ") + ` ORDER BY e.occurred_at, e.run_id, e.source_seq`
	if req.Limit > 0 {
		query += " LIMIT ?"
		args = append(args, req.Limit)
	}
	rows, err := db.readDB().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("rollup: query learning episodes: %w", err)
	}
	defer rows.Close()
	var out []LearningEpisode
	for rows.Next() {
		var e LearningEpisode
		var at string
		if err := rows.Scan(&e.RunID, &e.SourceSeq, &e.Workflow, &e.Stage, &e.Gate,
			&e.SourceAttempt, &e.NextAttempt, &e.WorkflowDigest, &e.GooberDigest,
			&e.EffectiveVersion, &e.Signature, &e.Classification, &e.EvidenceJSON,
			&e.CorrectionFeedback, &e.Outcome, &at); err != nil {
			return nil, fmt.Errorf("rollup: scan learning episode: %w", err)
		}
		var err error
		e.OccurredAt, err = parseTime(sql.NullString{String: at, Valid: at != ""})
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LearningClusters groups episodes by normalized signature for Tutor analysis.
func (db *DB) LearningClusters(ctx context.Context, req LearningEpisodeRequest) ([]LearningCluster, error) {
	episodes, err := db.LearningEpisodes(ctx, req)
	if err != nil {
		return nil, err
	}
	grouped := map[string]*LearningCluster{}
	for _, episode := range episodes {
		cluster := grouped[episode.Signature]
		if cluster == nil {
			cluster = &LearningCluster{Signature: episode.Signature, Classification: episode.Classification, RecommendedAction: recommendedLearningAction(episode.Classification)}
			grouped[episode.Signature] = cluster
		}
		cluster.Count++
		cluster.Episodes = append(cluster.Episodes, JournalPointer{RunID: episode.RunID})
	}
	out := make([]LearningCluster, 0, len(grouped))
	for _, cluster := range grouped {
		out = append(out, *cluster)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Signature < out[j].Signature
	})
	return out, nil
}
