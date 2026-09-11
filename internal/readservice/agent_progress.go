package readservice

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// AgentLifecycleStatus captures the latest durable lifecycle mark for one
// attempt-scoped agent invocation.
type AgentLifecycleStatus struct {
	Sequence  uint64                 `json:"sequence"`
	Lifecycle journal.AgentLifecycle `json:"lifecycle"`
	UpdatedAt time.Time              `json:"updatedAt,omitempty"`
}

// AgentCurrentStatus is the operator-facing current status card for one
// attempt-scoped agent invocation. Source is "lifecycle" or "progress".
type AgentCurrentStatus struct {
	Source    string                    `json:"source"`
	Sequence  uint64                    `json:"sequence"`
	Lifecycle journal.AgentLifecycle    `json:"lifecycle,omitempty"`
	Kind      journal.AgentProgressKind `json:"kind,omitempty"`
	Summary   string                    `json:"summary,omitempty"`
	UpdatedAt time.Time                 `json:"updatedAt,omitempty"`
}

// AgentProgressSummary describes the status card and ordered progress timeline for an agent invocation.
type AgentProgressSummary struct {
	AgentID      string                  `json:"agentId"`
	ParentID     string                  `json:"parentId,omitempty"`
	RunID        string                  `json:"runId"`
	Stage        string                  `json:"stage"`
	Attempt      int                     `json:"attempt"`
	Role         string                  `json:"role,omitempty"`
	Coordinator  bool                    `json:"coordinator,omitempty"`
	Worker       bool                    `json:"worker,omitempty"`
	Fidelity     string                  `json:"fidelity"`
	Degraded     bool                    `json:"degraded,omitempty"`
	DegradedText string                  `json:"degradedText,omitempty"`
	Lifecycle    *AgentLifecycleStatus   `json:"lifecycle,omitempty"`
	Current      *AgentCurrentStatus     `json:"currentStatus,omitempty"`
	Latest       *journal.AgentProgress  `json:"latest,omitempty"`
	History      []journal.AgentProgress `json:"history"`
	Children     []AgentProgressSummary  `json:"children,omitempty"`
}

// RunAgentProgress projects the current status and ordered progress timeline for all top-level and nested agents in a run.
func (s *Local) RunAgentProgress(ctx context.Context, runID string) ([]AgentProgressSummary, error) {
	run, err := s.openRun(runID)
	if err != nil {
		return nil, err
	}
	return summarizeAgentProgress(run.identity.RunID, run.records), nil
}

type progressKey struct {
	stage   string
	agentID string
	attempt int
}

func summarizeAgentProgress(runID string, records []journal.EventRecord) []AgentProgressSummary {
	records = latestPodAgentProgressRecords(records)
	agentMeta := make(map[progressKey]*journal.AgentProvenance)
	lifecycleMap := make(map[progressKey]*AgentLifecycleStatus)
	progressMap := make(map[progressKey][]journal.AgentProgress)
	metaOrder := make([]progressKey, 0)
	metaSeen := make(map[progressKey]bool)

	for _, record := range records {
		collectAgentProgressRecord(record.Event, runID, agentMeta, lifecycleMap, progressMap, &metaOrder, metaSeen)
	}

	summaries := make(map[progressKey]*AgentProgressSummary)
	for _, k := range metaOrder {
		summaries[k] = buildAgentProgressSummary(runID, k, agentMeta[k], lifecycleMap[k], progressMap[k])
	}
	return collectAgentProgressRoots(metaOrder, summaries)
}

func collectAgentProgressRecord(
	event journal.Event,
	runID string,
	agentMeta map[progressKey]*journal.AgentProvenance,
	lifecycleMap map[progressKey]*AgentLifecycleStatus,
	progressMap map[progressKey][]journal.AgentProgress,
	metaOrder *[]progressKey,
	metaSeen map[progressKey]bool,
) {
	if !event.KnownSchema() {
		return
	}
	switch event.Type {
	case journal.EventAgentLifecycle:
		collectAgentLifecycleRecord(event, agentMeta, lifecycleMap, metaOrder, metaSeen)
	case journal.EventAgentProgress:
		collectAgentProgressStatusRecord(event, runID, progressMap, metaOrder, metaSeen)
	}
}

func collectAgentLifecycleRecord(
	event journal.Event,
	agentMeta map[progressKey]*journal.AgentProvenance,
	lifecycleMap map[progressKey]*AgentLifecycleStatus,
	metaOrder *[]progressKey,
	metaSeen map[progressKey]bool,
) {
	if event.Agent == nil || event.Agent.ID == "" {
		return
	}
	key := progressKey{stage: event.Agent.Stage, agentID: event.Agent.ID, attempt: event.Agent.Attempt}
	noteProgressKey(key, metaOrder, metaSeen)
	copy := *event.Agent
	agentMeta[key] = &copy
	lifecycleMap[key] = &AgentLifecycleStatus{
		Sequence:  event.Seq,
		Lifecycle: event.Agent.Lifecycle,
		UpdatedAt: event.Agent.UpdatedAt,
	}
}

func collectAgentProgressStatusRecord(
	event journal.Event,
	runID string,
	progressMap map[progressKey][]journal.AgentProgress,
	metaOrder *[]progressKey,
	metaSeen map[progressKey]bool,
) {
	if event.Progress == nil || event.Progress.AgentID == "" {
		return
	}
	progress := normalizeAgentProgressRecord(event, runID)
	key := progressKey{stage: progress.Stage, agentID: progress.AgentID, attempt: progress.Attempt}
	noteProgressKey(key, metaOrder, metaSeen)
	progressMap[key] = append(progressMap[key], progress)
}

func noteProgressKey(key progressKey, metaOrder *[]progressKey, metaSeen map[progressKey]bool) {
	if metaSeen[key] {
		return
	}
	metaSeen[key] = true
	*metaOrder = append(*metaOrder, key)
}

func normalizeAgentProgressRecord(event journal.Event, runID string) journal.AgentProgress {
	progress := *event.Progress
	progress.RunID = firstNonEmpty(progress.RunID, runID)
	progress.Stage = firstNonEmpty(progress.Stage, event.Stage)
	if progress.Attempt < 1 {
		progress.Attempt = event.Attempt
	}
	progress.Sequence = event.Seq
	if progress.OccurredAt.IsZero() {
		progress.OccurredAt = event.Time
	}
	if progress.UpdatedAt.IsZero() {
		progress.UpdatedAt = progress.OccurredAt
	}
	return progress
}

func buildAgentProgressSummary(
	runID string,
	key progressKey,
	meta *journal.AgentProvenance,
	lifecycle *AgentLifecycleStatus,
	history []journal.AgentProgress,
) *AgentProgressSummary {
	sort.Slice(history, func(i, j int) bool {
		return history[i].Sequence < history[j].Sequence
	})
	history = retainRecentAgentProgress(history)
	summary := &AgentProgressSummary{
		AgentID:  key.agentID,
		RunID:    runID,
		Stage:    key.stage,
		Attempt:  key.attempt,
		History:  history,
		Children: []AgentProgressSummary{},
	}
	applyAgentProgressMeta(summary, meta)
	applyLatestProgress(summary, history)
	summary.Lifecycle = lifecycle
	summary.Current = summarizeCurrentStatus(summary.Lifecycle, summary.Latest, summary.Fidelity)
	applyAgentProgressDegraded(summary, len(history) == 0)
	return summary
}

func applyAgentProgressMeta(summary *AgentProgressSummary, meta *journal.AgentProvenance) {
	if meta == nil {
		return
	}
	summary.ParentID = meta.ParentID
	summary.Stage = meta.Stage
	summary.Coordinator = meta.Coordinator
	summary.Worker = meta.Worker
	switch {
	case meta.Coordinator:
		summary.Role = "coordinator"
	case meta.Worker:
		summary.Role = "worker"
	case meta.Leaf:
		summary.Role = "leaf"
	}
	summary.Fidelity = meta.Fidelity
}

func applyLatestProgress(summary *AgentProgressSummary, history []journal.AgentProgress) {
	if len(history) == 0 {
		return
	}
	summary.Stage = history[0].Stage
	latest := history[len(history)-1]
	summary.Latest = &latest
	summary.Fidelity = firstNonEmpty(summary.Fidelity, latest.Fidelity, journal.AgentFidelityFull)
}

func applyAgentProgressDegraded(summary *AgentProgressSummary, emptyHistory bool) {
	if summary.Fidelity != "" && summary.Fidelity != journal.AgentFidelityNone && !emptyHistory {
		return
	}
	if emptyHistory {
		summary.Fidelity = journal.AgentFidelityNone
	}
	summary.Degraded = true
	summary.DegradedText = "structured progress unavailable (degraded to tool/transcript activity)"
}

func collectAgentProgressRoots(
	metaOrder []progressKey,
	summaries map[progressKey]*AgentProgressSummary,
) []AgentProgressSummary {
	children := make(map[progressKey][]progressKey)
	for _, key := range metaOrder {
		summary := summaries[key]
		if summary == nil || summary.ParentID == "" {
			continue
		}
		parentKey := progressKey{stage: summary.Stage, agentID: summary.ParentID, attempt: summary.Attempt}
		if summaries[parentKey] != nil {
			children[parentKey] = append(children[parentKey], key)
		}
	}
	var roots []AgentProgressSummary
	for _, key := range metaOrder {
		if _, ok := agentProgressRoot(key, summaries); ok {
			roots = append(roots, materializeAgentProgressSummary(key, summaries, children, make(map[progressKey]bool)))
		}
	}
	return roots
}

func materializeAgentProgressSummary(
	key progressKey,
	summaries map[progressKey]*AgentProgressSummary,
	children map[progressKey][]progressKey,
	visiting map[progressKey]bool,
) AgentProgressSummary {
	summary := summaries[key]
	if summary == nil {
		return AgentProgressSummary{}
	}
	copy := *summary
	copy.Children = nil
	if visiting[key] {
		return copy
	}
	visiting[key] = true
	for _, childKey := range children[key] {
		copy.Children = append(copy.Children, materializeAgentProgressSummary(childKey, summaries, children, visiting))
	}
	delete(visiting, key)
	return copy
}

func agentProgressRoot(
	key progressKey,
	summaries map[progressKey]*AgentProgressSummary,
) (AgentProgressSummary, bool) {
	summary := summaries[key]
	if summary == nil {
		return AgentProgressSummary{}, false
	}
	parentKey := progressKey{stage: summary.Stage, agentID: summary.ParentID, attempt: summary.Attempt}
	if summary.ParentID != "" && summaries[parentKey] != nil {
		return AgentProgressSummary{}, false
	}
	return *summary, true
}

func retainRecentAgentProgress(history []journal.AgentProgress) []journal.AgentProgress {
	if len(history) <= journal.AgentProgressRetainedHistory {
		return history
	}
	start := len(history) - journal.AgentProgressRetainedHistory
	return append([]journal.AgentProgress(nil), history[start:]...)
}

func summarizeCurrentStatus(lifecycle *AgentLifecycleStatus, latest *journal.AgentProgress, fidelity string) *AgentCurrentStatus {
	switch {
	case latest != nil && (lifecycle == nil || latest.Sequence >= lifecycle.Sequence):
		return &AgentCurrentStatus{
			Source:    "progress",
			Sequence:  latest.Sequence,
			Kind:      latest.Kind,
			Summary:   progressSummary(*latest),
			UpdatedAt: latest.UpdatedAt,
		}
	case lifecycle != nil:
		return &AgentCurrentStatus{
			Source:    "lifecycle",
			Sequence:  lifecycle.Sequence,
			Lifecycle: lifecycle.Lifecycle,
			Summary:   lifecycleSummary(lifecycle.Lifecycle, fidelity),
			UpdatedAt: lifecycle.UpdatedAt,
		}
	default:
		return nil
	}
}

func progressSummary(progress journal.AgentProgress) string {
	switch {
	case strings.TrimSpace(progress.Summary) != "":
		return strings.TrimSpace(progress.Summary)
	case len(progress.Progress) > 0:
		return strings.Join(progress.Progress, "; ")
	case strings.TrimSpace(progress.Decision) != "":
		return strings.TrimSpace(progress.Decision)
	case strings.TrimSpace(progress.Blocker) != "":
		return strings.TrimSpace(progress.Blocker)
	case strings.TrimSpace(progress.Question) != "":
		return strings.TrimSpace(progress.Question)
	case strings.TrimSpace(progress.NextAction) != "":
		return strings.TrimSpace(progress.NextAction)
	case len(progress.Plan) > 0:
		return strings.Join(progress.Plan, "; ")
	default:
		return string(progress.Kind)
	}
}

func lifecycleSummary(lifecycle journal.AgentLifecycle, fidelity string) string {
	switch lifecycle {
	case journal.AgentWaiting:
		return "Waiting for more input or tool activity."
	case journal.AgentResumed:
		return "Resumed work after new activity."
	case journal.AgentCompleted:
		return "Completed the current attempt."
	case journal.AgentFailed:
		return "Failed during the current attempt."
	case journal.AgentCancelled:
		return "Cancelled before completing the current attempt."
	default:
		if fidelity == journal.AgentFidelityNone {
			return "Running without structured progress; showing lifecycle-only status."
		}
		return "Running."
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func latestPodAgentProgressRecords(records []journal.EventRecord) []journal.EventRecord {
	latest := make(map[progressKey]uint64)
	for _, record := range records {
		key, ok := progressIdentityForEvent(record.Event)
		if !ok {
			continue
		}
		if ordinal := progressEventPodOrdinal(record.Event); ordinal > latest[key] {
			latest[key] = ordinal
		}
	}
	filtered := make([]journal.EventRecord, 0, len(records))
	for _, record := range records {
		key, ok := progressIdentityForEvent(record.Event)
		if ok {
			if ordinal := progressEventPodOrdinal(record.Event); ordinal > 0 && latest[key] > 0 && latest[key] != ordinal {
				continue
			}
		}
		filtered = append(filtered, record)
	}
	return filtered
}

func progressIdentityForEvent(event journal.Event) (progressKey, bool) {
	switch event.Type {
	case journal.EventAgentLifecycle:
		if event.Agent == nil || event.Agent.Stage == "" || event.Agent.ID == "" || event.Agent.Attempt < 1 {
			return progressKey{}, false
		}
		return progressKey{stage: event.Agent.Stage, agentID: event.Agent.ID, attempt: event.Agent.Attempt}, true
	case journal.EventAgentProgress:
		if event.Progress == nil || event.Progress.AgentID == "" {
			return progressKey{}, false
		}
		stage := firstNonEmpty(event.Progress.Stage, event.Stage)
		attempt := event.Progress.Attempt
		if attempt < 1 {
			attempt = event.Attempt
		}
		if stage == "" || attempt < 1 {
			return progressKey{}, false
		}
		return progressKey{stage: stage, agentID: event.Progress.AgentID, attempt: attempt}, true
	default:
		return progressKey{}, false
	}
}

func progressEventPodOrdinal(event journal.Event) uint64 {
	key, _ := event.Runner["emitKey"].(string)
	rest, ok := strings.CutPrefix(key, "pod/")
	if !ok {
		return 0
	}
	ordinal, operation, ok := strings.Cut(rest, "/")
	if !ok || operation == "" {
		return 0
	}
	n, err := strconv.ParseUint(ordinal, 10, 64)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != ordinal {
		return 0
	}
	return n
}
