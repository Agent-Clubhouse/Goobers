package featureusage

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/goobers/goobers/internal/journal"
)

// Scan limits bound work and retained data per gaggle observation.
const (
	MaxRuns       = 128
	MaxScanBytes  = 8 << 20
	MaxRunBytes   = 1 << 20
	MaxEventBytes = 64 << 10
)

// Count is an absolute count for one observation window. Complete is required
// before a zero can be interpreted as unused; positive partial counts are lower
// bounds on observed use, not an estimate of unobserved invocations.
type Count struct {
	Value    int64
	Complete bool
}

// IDs returns the closed catalogue. Unknown adapters and arbitrary capabilities
// are deliberately not exported as user-controlled feature dimensions.
func IDs() []string {
	return []string{"runner.local", "runner.engine", "adapter.copilot", "adapter.claude", "adapter.codex", "provider.github", "provider.gitea", "provider.azure-devops", "dsl.v1", "dsl.v2", "dsl.v3", "capability.nested-agents"}
}

// Scan reads an explicitly bounded journal window. It never migrates journals,
// follows arbitrary artifact paths, or exports raw event fields. Re-scanning the
// same sequence produces the same absolute count; no per-poll summation occurs.
func Scan(ctx context.Context, runsDir string, start, end time.Time) map[string]Count {
	result := map[string]Count{}
	for _, id := range IDs() {
		result[id] = Count{Complete: !strings.HasPrefix(id, "provider.")}
	}
	if ctx.Err() != nil {
		return incomplete(result)
	}
	dir, err := os.Open(runsDir)
	if err != nil {
		return incomplete(result)
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(MaxRuns + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return incomplete(result)
	}
	if len(entries) > MaxRuns {
		incomplete(result)
		entries = entries[:MaxRuns]
	}
	budget := int64(MaxScanBytes)
	for _, entry := range entries {
		if ctx.Err() != nil {
			return incomplete(result)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			incomplete(result)
			continue
		}
		if !entry.IsDir() {
			continue
		}
		if err := scanRun(ctx, filepath.Join(runsDir, entry.Name()), start, end, &budget, result); err != nil {
			result = incomplete(result)
		}
	}
	return result
}
func incomplete(result map[string]Count) map[string]Count {
	for id, count := range result {
		count.Complete = false
		result[id] = count
	}
	return result
}
func increment(result map[string]Count, id string, n int64) {
	count, ok := result[id]
	if !ok || n < 0 || n > 1000000 {
		return
	}
	count.Value += n
	result[id] = count
}
func partialDynamic(result map[string]Count) {
	for id, count := range result {
		if strings.HasPrefix(id, "adapter.") || strings.HasPrefix(id, "provider.") || strings.HasPrefix(id, "capability.") {
			count.Complete = false
			result[id] = count
		}
	}
}

func scanRun(ctx context.Context, dir string, start, end time.Time, budget *int64, result map[string]Count) error {
	data, err := boundedFile(filepath.Join(dir, "run.yaml"), 64<<10, budget)
	if err != nil {
		return err
	}
	var identity journal.RunIdentity
	if err := yaml.Unmarshal(data, &identity); err != nil || !identity.KnownSchema() {
		return errors.New("unknown run identity")
	}
	data, err = boundedFile(filepath.Join(dir, "events.jsonl"), MaxRunBytes, budget)
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 4096), MaxEventBytes)
	children := map[string]bool{}
	var lastSeq uint64
	terminal := false
	relevant := false
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var event journal.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || !event.KnownSchema() || event.Seq <= lastSeq {
			return errors.New("incomplete event sequence")
		}
		if event.Time.IsZero() || event.Time.After(end) || event.RunID != "" && event.RunID != identity.RunID {
			return errors.New("event clock or identity unknown")
		}
		lastSeq = event.Seq
		if event.Type == journal.EventRunStarted || event.Type == journal.EventRunResumed {
			terminal = false
		}
		if event.Type == journal.EventRunFinished {
			terminal = true
		}
		if event.Time.Before(start) || event.Time.After(end) {
			continue
		}
		relevant = true
		if event.Type == journal.EventRunStarted {
			observeRun(dir, identity, result, budget)
		}
		observeEvent(event, result)
		observeChild(event, children, result)
	}
	if !terminal || relevant {
		partialDynamic(result)
	}
	return scanner.Err()
}
func observeEvent(event journal.Event, result map[string]Count) {
	if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != Kind {
		return
	}
	id, _ := event.Runner["featureId"].(string)
	number, ok := event.Runner["count"].(float64)
	if !ok || number != float64(int64(number)) {
		return
	}
	increment(result, id, int64(number))
}
func observeRun(dir string, identity journal.RunIdentity, result map[string]Count, budget *int64) {
	switch identity.Driver {
	case "":
		increment(result, "runner.local", 1)
	case journal.DriverEngine:
		increment(result, "runner.engine", 1)
	default:
		incomplete(result)
	}
	for _, input := range identity.Inputs {
		if input.Name == journal.PinnedWorkflowDefinitionInputName {
			max := int64(MaxRunBytes)
			if *budget < max {
				max = *budget
			}
			if max <= 0 {
				incomplete(result)
				return
			}
			data, err := pinnedBytes(dir, input.Ref, max)
			*budget -= int64(len(data))
			var definition struct {
				DSLVersion string `json:"dslVersion"`
			}
			if err != nil || json.Unmarshal(data, &definition) != nil {
				incomplete(result)
				return
			}
			switch definition.DSLVersion {
			case "1.4":
				increment(result, "dsl.v1", 1)
			case "2.0":
				increment(result, "dsl.v2", 1)
			case "3.0":
				increment(result, "dsl.v3", 1)
			default:
				incomplete(result)
			}
			return
		}
	}
	incomplete(result)
}
func boundedFile(path string, limit int64, budget *int64) ([]byte, error) {
	if *budget < limit {
		limit = *budget
	}
	if limit <= 0 {
		return nil, errors.New("scan budget exceeded")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("file outside scan bounds")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	*budget -= int64(len(data))
	if int64(len(data)) > limit {
		return nil, errors.New("file grew beyond scan bounds")
	}
	return data, err
}

func observeChild(event journal.Event, seen map[string]bool, result map[string]Count) {
	agent := event.Agent
	if event.Type != journal.EventAgentLifecycle || agent == nil || agent.Lifecycle != journal.AgentStarted || agent.ParentID == "" {
		return
	}
	if agent.Schema != "goobers.dev/journal/agent/v1" || agent.ID == "" || agent.Attempt < 1 || len(seen) >= 4096 {
		partialDynamic(result)
		return
	}
	key := fmt.Sprintf("%s/%s/%d", agent.Stage, agent.ID, agent.Attempt)
	if !seen[key] {
		seen[key] = true
		increment(result, "capability.nested-agents", 1)
	}
}

func pinnedBytes(dir string, ref journal.Ref, limit int64) ([]byte, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	file, err := root.Open(ref.Path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("pinned definition exceeds bounds")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit || journal.Digest(data) != ref.Digest {
		return nil, errors.New("pinned definition size or digest mismatch")
	}
	return data, nil
}
