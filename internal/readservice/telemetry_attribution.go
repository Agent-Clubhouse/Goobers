package readservice

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/learning"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const attributionListPageSize = 200

var interventionEvidencePattern = regexp.MustCompile(`^intervention: stage (.+) attempt ([0-9]+) failed and attempt ([0-9]+) succeeded$`)

// AgentInvocationReader supplies model and harness provenance for a run.
type AgentInvocationReader interface {
	AgentInvocations(context.Context, string) ([]rollup.AgentInvocation, error)
}

// StoredAttributionQuery scopes the stored run evidence to aggregate.
type StoredAttributionQuery struct {
	Gaggle   string
	Workflow string
	Since    time.Time
	Until    time.Time
}

// StoredAttributionCohorts reconstructs per-run credit attribution from
// journaled span and artifact evidence, then re-aggregates it by cohort.
func StoredAttributionCohorts(
	ctx context.Context,
	root string,
	reads readmodel.Reader,
	invocations AgentInvocationReader,
	query StoredAttributionQuery,
) ([]creditgraph.CohortAggregation, error) {
	observations, err := storedAttributionObservations(ctx, root, reads, invocations, query)
	if err != nil {
		return nil, err
	}
	return creditgraph.AggregateAttributionEvidence(observations), nil
}

func storedAttributionObservations(
	ctx context.Context,
	root string,
	reads readmodel.Reader,
	invocations AgentInvocationReader,
	query StoredAttributionQuery,
) ([]creditgraph.AttributionObservation, error) {
	if reads == nil || invocations == nil || strings.TrimSpace(root) == "" {
		return nil, nil
	}
	layout := instance.NewLayout(root)
	rows, err := adverseRuns(ctx, reads, query)
	if err != nil {
		return nil, err
	}
	observations := make([]creditgraph.AttributionObservation, 0, len(rows))
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		observation, ok, err := storedAttributionObservation(ctx, layout, invocations, row)
		if err != nil {
			return nil, err
		}
		if ok {
			observations = append(observations, observation)
		}
	}
	return observations, nil
}

func adverseRuns(ctx context.Context, reads readmodel.Reader, query StoredAttributionQuery) ([]readmodel.RunRow, error) {
	options := readmodel.ListOptions{
		Gaggle:   query.Gaggle,
		Workflow: query.Workflow,
		Since:    query.Since,
		Until:    query.Until,
		Limit:    attributionListPageSize,
	}
	var rows []readmodel.RunRow
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := reads.ListRuns(ctx, options)
		if err != nil {
			return nil, fmt.Errorf("list attribution runs: %w", err)
		}
		for _, row := range page.Runs {
			if isAdverseAttributedRun(row) {
				rows = append(rows, row)
			}
		}
		if !page.HasMore || page.Next.Zero() {
			return rows, nil
		}
		options.Cursor = page.Next
	}
}

func isAdverseAttributedRun(row readmodel.RunRow) bool {
	if !row.Terminal {
		return false
	}
	if row.OutcomeTarget == "@abort" || row.OutcomeTarget == "@escalate" {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(row.OutcomeVerdict)) {
	case "fail", "failure", "reject", "rejected", "needs-changes":
		return true
	default:
		return row.Phase == journal.PhaseEscalated
	}
}

func storedAttributionObservation(
	ctx context.Context,
	layout instance.Layout,
	invocations AgentInvocationReader,
	row readmodel.RunRow,
) (creditgraph.AttributionObservation, bool, error) {
	runDir, err := layout.FindRunDir(row.RunID)
	if err != nil {
		return creditgraph.AttributionObservation{}, false, fmt.Errorf("open attribution run %q: %w", row.RunID, err)
	}
	reader, err := journal.OpenRead(runDir)
	if err != nil {
		return creditgraph.AttributionObservation{}, false, fmt.Errorf("open attribution journal %q: %w", row.RunID, err)
	}
	records, err := reader.EventRecords()
	if err != nil {
		return creditgraph.AttributionObservation{}, false, fmt.Errorf("read attribution journal %q: %w", row.RunID, err)
	}
	events := make([]journal.Event, len(records))
	for i := range records {
		events[i] = records[i].Event
	}
	identity, err := reader.Identity()
	if err != nil {
		return creditgraph.AttributionObservation{}, false, fmt.Errorf("read attribution identity %q: %w", row.RunID, err)
	}
	spanData := map[string][]byte{}
	for _, event := range events {
		if event.Type != journal.EventSpanRecorded || event.Ref == nil || event.DataSchema != telemetry.GenAIEventSchema {
			continue
		}
		data, err := reader.SpanBytes(*event.Ref)
		if err != nil {
			return creditgraph.AttributionObservation{}, false, fmt.Errorf("read attribution span %q/%d: %w", row.RunID, event.Seq, err)
		}
		spanData[event.Ref.Digest] = data
	}
	graph, err := creditgraph.Build(creditgraph.Input{
		RunID:    identity.RunID,
		Gaggle:   identity.Gaggle,
		Workflow: identity.Workflow,
		Events:   events,
		SpanData: spanData,
	})
	if err != nil {
		return creditgraph.AttributionObservation{}, false, fmt.Errorf("build attribution graph %q: %w", row.RunID, err)
	}
	attribution := creditgraph.Attribute(graph)
	runInvocations, err := invocations.AgentInvocations(ctx, row.RunID)
	if err != nil {
		return creditgraph.AttributionObservation{}, false, fmt.Errorf("load attribution provenance %q: %w", row.RunID, err)
	}
	return creditgraph.AttributionObservation{
		RunID:            row.RunID,
		EffectiveVersion: effectiveVersionHash(row, runInvocations),
		Workload:         workloadKey(row),
		Attribution:      attribution,
		Evidence:         buildAttributionEvidence(layout.Root, runDir, records, graph, attribution),
	}, true, nil
}

func effectiveVersionHash(row readmodel.RunRow, invocations []rollup.AgentInvocation) string {
	model, harness := singleModelHarness(invocations)
	version := rollup.EffectiveVersion{
		WorkflowDigest: row.WorkflowDigest,
		GooberDigest:   row.GooberDigest,
		Model:          model,
		HarnessVersion: harness,
	}
	if version.WorkflowDigest == "" && version.GooberDigest == "" && version.Model == "" && version.HarnessVersion == "" {
		return learning.EffectiveVersion(row.WorkflowDigest, row.GooberDigest)
	}
	return version.Hash()
}

func singleModelHarness(invocations []rollup.AgentInvocation) (string, string) {
	type pair struct {
		model   string
		harness string
	}
	pairs := map[pair]struct{}{}
	for _, invocation := range invocations {
		pairs[pair{
			model:   strings.TrimSpace(invocation.Model),
			harness: strings.TrimSpace(invocation.HarnessVersion),
		}] = struct{}{}
	}
	if len(pairs) != 1 {
		return "", ""
	}
	for candidate := range pairs {
		return candidate.model, candidate.harness
	}
	return "", ""
}

func workloadKey(row readmodel.RunRow) string {
	if strings.TrimSpace(row.TriggerKind) != "" {
		return row.TriggerKind
	}
	return "unknown"
}

type attributionEventIndex struct {
	runID       string
	journalPath string
	runEvent    *journal.Event
	stageEvents map[string]journal.Event
	stageTries  map[string]map[int]journal.Event
	artifactBy  map[string]journal.Event
	spanBy      map[string]journal.Event
	agentByID   map[string]journal.Event
	gateByID    map[string]journal.Event
}

func buildAttributionEvidence(
	root string,
	runDir string,
	records []journal.EventRecord,
	graph *creditgraph.Graph,
	attribution creditgraph.Attribution,
) []creditgraph.AttributionEvidenceLink {
	index := buildAttributionEventIndex(root, runDir, records)
	evidence := make([]creditgraph.AttributionEvidenceLink, 0, len(attribution.Contributions)+len(attribution.Causes))
	for _, contribution := range attribution.Contributions {
		node, _ := graph.Node(contribution.NodeID)
		if link, ok := evidenceLinkForNode(
			index,
			node,
			contribution.Stage,
			fmt.Sprintf("share=%s, confidence=%s", formatAttributionValue(contribution.Share), formatAttributionValue(contribution.Confidence)),
			"contribution",
		); ok {
			evidence = append(evidence, link)
		}
	}
	for _, cause := range attribution.Causes {
		node, _ := graph.Node(cause.NodeID)
		for _, detail := range cause.Evidence {
			if matches := interventionEvidenceLinks(index, node, cause.Stage, detail, string(cause.Class)); len(matches) > 0 {
				evidence = append(evidence, matches...)
				continue
			}
			if link, ok := evidenceLinkForNode(index, node, cause.Stage, detail, string(cause.Class)); ok {
				evidence = append(evidence, link)
			}
		}
	}
	return evidence
}

func buildAttributionEventIndex(root, runDir string, records []journal.EventRecord) attributionEventIndex {
	journalPath := "events.jsonl"
	if relRun, err := filepath.Rel(root, runDir); err == nil {
		journalPath = filepath.ToSlash(filepath.Join(relRun, "events.jsonl"))
	}
	index := attributionEventIndex{
		runID:       filepath.Base(runDir),
		journalPath: journalPath,
		stageEvents: map[string]journal.Event{},
		stageTries:  map[string]map[int]journal.Event{},
		artifactBy:  map[string]journal.Event{},
		spanBy:      map[string]journal.Event{},
		agentByID:   map[string]journal.Event{},
		gateByID:    map[string]journal.Event{},
	}
	for _, record := range records {
		event := record.Event
		switch event.Type {
		case journal.EventRunFinished:
			index.runEvent = cloneEvent(event)
		case journal.EventGateEvaluated, journal.EventGateOverridden:
			index.gateByID[evaluatorNodeID(event)] = event
		case journal.EventStageFinished:
			index.stageEvents[stageAttemptKey(event.Stage, event.Attempt)] = event
			if index.stageTries[event.Stage] == nil {
				index.stageTries[event.Stage] = map[int]journal.Event{}
			}
			index.stageTries[event.Stage][event.Attempt] = event
		case journal.EventAgentLifecycle:
			if event.Agent != nil && strings.TrimSpace(event.Agent.ID) != "" {
				index.agentByID[event.Agent.ID] = event
			}
		case journal.EventStageStarted:
			key := stageAttemptKey(event.Stage, event.Attempt)
			if _, exists := index.stageEvents[key]; !exists {
				index.stageEvents[key] = event
			}
			if index.stageTries[event.Stage] == nil {
				index.stageTries[event.Stage] = map[int]journal.Event{}
			}
			if _, exists := index.stageTries[event.Stage][event.Attempt]; !exists {
				index.stageTries[event.Stage][event.Attempt] = event
			}
		case journal.EventArtifactRecorded:
			index.artifactBy[artifactKey(event.Stage, event.Attempt, event.Name, digestOf(event.Ref))] = event
			key := stageAttemptKey(event.Stage, event.Attempt)
			if _, exists := index.artifactBy[key]; !exists {
				index.artifactBy[key] = event
			}
			digest := digestOf(event.Ref)
			if digest != "" {
				index.artifactBy[digest] = event
			}
		case journal.EventSpanRecorded:
			key := stageAttemptKey(event.Stage, event.Attempt)
			if _, exists := index.spanBy[key]; !exists {
				index.spanBy[key] = event
			}
			digest := digestOf(event.Ref)
			if digest != "" {
				index.spanBy[digest] = event
			}
		}
	}
	return index
}

func interventionEvidenceLinks(index attributionEventIndex, node creditgraph.Node, stage, detail, source string) []creditgraph.AttributionEvidenceLink {
	match := interventionEvidencePattern.FindStringSubmatch(strings.TrimSpace(detail))
	if len(match) != 4 {
		return nil
	}
	failedAttempt := parsePositiveInt(match[2])
	succeededAttempt := parsePositiveInt(match[3])
	var links []creditgraph.AttributionEvidenceLink
	for _, attempt := range []int{failedAttempt, succeededAttempt} {
		if attempt < 1 {
			continue
		}
		event, ok := index.stageTries[match[1]][attempt]
		if !ok {
			continue
		}
		links = append(links, exactEvidenceLink(index, event, node.ID, stage, detail, source))
	}
	return links
}

func evidenceLinkForNode(index attributionEventIndex, node creditgraph.Node, stage, detail, source string) (creditgraph.AttributionEvidenceLink, bool) {
	if event, ok := eventForNode(index, node); ok {
		return exactEvidenceLink(index, event, node.ID, stage, detail, source), true
	}
	return creditgraph.AttributionEvidenceLink{}, false
}

func eventForNode(index attributionEventIndex, node creditgraph.Node) (journal.Event, bool) {
	return exactNodeEvent(index, node)
}

func exactNodeEvent(index attributionEventIndex, node creditgraph.Node) (journal.Event, bool) {
	switch node.Kind {
	case creditgraph.KindEvidence:
		if event, ok := index.artifactBy[artifactKey(node.Stage, node.Attempt, node.Label, node.Attributes["digest"])]; ok {
			return event, true
		}
	case creditgraph.KindStage:
		if event, ok := index.stageEvents[stageAttemptKey(node.Stage, node.Attempt)]; ok {
			return event, true
		}
	case creditgraph.KindRun, creditgraph.KindOutcome:
		if index.runEvent != nil {
			return *index.runEvent, true
		}
	case creditgraph.KindEvaluator:
		if event, ok := index.gateByID[node.ID]; ok {
			return event, true
		}
	case creditgraph.KindSubagent:
		if event, ok := index.agentByID[node.Label]; ok {
			return event, true
		}
	case creditgraph.KindModelInvocation, creditgraph.KindToolCall, creditgraph.KindToolResult:
		if digest := nodeSpanDigest(node.ID); digest != "" {
			if event, ok := index.spanBy[digest]; ok {
				return event, true
			}
		}
		if agentID := lifecycleAgentID(node); agentID != "" {
			if event, ok := index.agentByID[agentID]; ok {
				return event, true
			}
		}
	}
	return journal.Event{}, false
}

func exactEvidenceLink(index attributionEventIndex, event journal.Event, nodeID, stage, detail, source string) creditgraph.AttributionEvidenceLink {
	link := creditgraph.AttributionEvidenceLink{
		RunID:           index.runID,
		NodeID:          nodeID,
		Stage:           stageOrEventStage(stage, event.Stage),
		Detail:          detail,
		Source:          source,
		JournalSequence: int64(event.Seq),
		JournalPath:     index.journalPath,
	}
	if event.Ref != nil {
		link.ArtifactPath = event.Ref.Path
		link.ArtifactDigest = event.Ref.Digest
		link.ArtifactMediaType = event.Ref.MediaType
	}
	if link.Stage == "" {
		link.Stage = event.Stage
	}
	return link
}

func evaluatorNodeID(event journal.Event) string {
	return fmt.Sprintf("evaluator:%s#%d", event.Gate, event.Seq)
}

func nodeSpanDigest(nodeID string) string {
	hash := strings.LastIndex(nodeID, "#")
	if hash <= 0 {
		return ""
	}
	at := strings.LastIndex(nodeID[:hash], "@")
	if at <= 0 || at+1 >= hash {
		return ""
	}
	return nodeID[at+1 : hash]
}

func lifecycleAgentID(node creditgraph.Node) string {
	if strings.HasPrefix(node.ID, "model:") {
		return strings.TrimPrefix(node.ID, "model:")
	}
	if strings.HasPrefix(node.ID, "subagent:") {
		return strings.TrimPrefix(node.ID, "subagent:")
	}
	return ""
}

func stageOrEventStage(stage, eventStage string) string {
	if strings.TrimSpace(stage) != "" {
		return stage
	}
	return eventStage
}

func artifactKey(stage string, attempt int, name, digest string) string {
	if digest != "" {
		return stageAttemptKey(stage, attempt) + "|" + name + "|" + digest
	}
	return stageAttemptKey(stage, attempt) + "|" + name
}

func stageAttemptKey(stage string, attempt int) string {
	if attempt < 1 {
		attempt = 1
	}
	return fmt.Sprintf("%s#%d", stage, attempt)
}

func digestOf(ref *journal.Ref) string {
	if ref == nil {
		return ""
	}
	return ref.Digest
}

func cloneEvent(event journal.Event) *journal.Event {
	copy := event
	return &copy
}

func parsePositiveInt(raw string) int {
	value := 0
	for _, r := range raw {
		if r < '0' || r > '9' {
			return 0
		}
		value = value*10 + int(r-'0')
	}
	return value
}

func formatAttributionValue(value float64) string {
	return fmt.Sprintf("%.6f", value)
}
