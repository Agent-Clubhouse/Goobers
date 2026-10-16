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

func attributionEvalPath(root string) string {
	return filepath.Join(instance.NewLayout(root).SchedulerDir(), "backprop-audit", "attribution-eval.json")
}

// PublishAttributionEval records an attribution eval summary for the instance
// at root, replacing any earlier one. Telemetry read surfaces report it and the
// fault auditor derives its filing confidence threshold from its calibration.
func PublishAttributionEval(root string, summary creditgraph.AttributionEvalSummary) error {
	if strings.TrimSpace(root) == "" {
		return errors.New("publish attribution eval: instance root is required")
	}
	if err := summary.Validate(); err != nil {
		return fmt.Errorf("publish attribution eval: %w", err)
	}
	data, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("encode attribution eval: %w", err)
	}
	path := attributionEvalPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create attribution eval directory: %w", err)
	}
	if err := journal.WriteFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write attribution eval: %w", err)
	}
	return nil
}

// ReadAttributionEval returns the instance's published attribution eval
// summary, or nil when none has been published. A present but malformed
// summary is an error rather than a silent fallback to the default threshold.
func ReadAttributionEval(root string) (*creditgraph.AttributionEvalSummary, error) {
	if strings.TrimSpace(root) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(attributionEvalPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read attribution eval: %w", err)
	}
	var summary creditgraph.AttributionEvalSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return nil, fmt.Errorf("decode attribution eval: %w", err)
	}
	if err := summary.Validate(); err != nil {
		return nil, fmt.Errorf("decode attribution eval: %w", err)
	}
	return &summary, nil
}
