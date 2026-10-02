package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.temporal.io/sdk/converter"

	"github.com/goobers/goobers/internal/journal"
)

const projectionDeadLetterDirectory = ".engine-projection-deadletters"

// ProjectionDeadLetter is the durable operator record of a closed execution
// whose immutable history cannot produce a terminal journal. Removing its file
// explicitly retries projection (for example after upgrading the projector).
type ProjectionDeadLetter struct {
	Namespace   string    `json:"namespace"`
	WorkflowID  string    `json:"workflowId"`
	ExecutionID string    `json:"executionId"`
	Gaggle      string    `json:"gaggle"`
	Status      string    `json:"status"`
	Reason      string    `json:"reason"`
	RecordedAt  time.Time `json:"recordedAt"`
}

func projectionExecutionKey(namespace, workflowID, executionID string) string {
	// JSON framing avoids delimiter ambiguity; hashing also keeps all Temporal
	// identity strings out of filesystem path segments.
	encoded, _ := json.Marshal([]string{namespace, workflowID, executionID})
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func projectionDeadLetterExists(runsDir, key string) (bool, error) {
	path := filepath.Join(runsDir, projectionDeadLetterDirectory, key+".json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("engine: read projection dead letter: %w", err)
	}
	var record ProjectionDeadLetter
	if err := json.Unmarshal(data, &record); err != nil {
		return false, fmt.Errorf("engine: decode projection dead letter %s: %w", path, err)
	}
	if record.ExecutionID == "" || record.Reason == "" || projectionExecutionKey(record.Namespace, record.WorkflowID, record.ExecutionID) != key {
		return false, fmt.Errorf("engine: invalid projection dead letter %s", path)
	}
	return true, nil
}

func writeProjectionDeadLetter(runsDir, key string, record ProjectionDeadLetter) (string, error) {
	dir := filepath.Join(runsDir, projectionDeadLetterDirectory)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", fmt.Errorf("engine: create projection dead-letter directory: %w", err)
	}
	record.RecordedAt = time.Now().UTC()
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, key+".json")
	if err := journal.WriteFileAtomic(path, append(data, '\n'), 0600); err != nil {
		return "", fmt.Errorf("engine: persist projection dead letter: %w", err)
	}
	return path, nil
}

// All queries during a reconciliation pass target the SAME execution listed
// by visibility, even if the workflow ID has since been reused for a new run.
type executionProjectionQuerier struct {
	projectionQuerier
	executionID string
}

func (q executionProjectionQuerier) QueryWorkflow(ctx context.Context, workflowID, _, query string, args ...interface{}) (converter.EncodedValue, error) {
	return q.projectionQuerier.QueryWorkflow(ctx, workflowID, q.executionID, query, args...)
}
