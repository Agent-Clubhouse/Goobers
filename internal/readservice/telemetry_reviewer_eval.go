package readservice

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func reviewerEvalPath(root string) string {
	return filepath.Join(instance.NewLayout(root).SchedulerDir(), "backprop-audit", "reviewer-eval.json")
}

// PublishReviewerEval records a merge-review recall/precision eval summary for
// the instance at root, replacing any earlier one. Telemetry read surfaces
// report it.
func PublishReviewerEval(root string, summary creditgraph.ReviewerEvalSummary) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("publish reviewer eval: instance root is required")
	}
	if err := summary.Validate(); err != nil {
		return fmt.Errorf("publish reviewer eval: %w", err)
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("encode reviewer eval: %w", err)
	}
	path := reviewerEvalPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create reviewer eval directory: %w", err)
	}
	if err := journal.WriteFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write reviewer eval: %w", err)
	}
	return nil
}

// ReadReviewerEval returns the instance's published reviewer eval summary, or
// nil when none has been published. A present but malformed summary is an
// error, never reported as if it were a measured score.
func ReadReviewerEval(root string) (*creditgraph.ReviewerEvalSummary, error) {
	if strings.TrimSpace(root) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(reviewerEvalPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read reviewer eval: %w", err)
	}
	var summary creditgraph.ReviewerEvalSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return nil, fmt.Errorf("decode reviewer eval: %w", err)
	}
	if err := summary.Validate(); err != nil {
		return nil, fmt.Errorf("decode reviewer eval: %w", err)
	}
	return &summary, nil
}
