package readservice

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	platformlock "github.com/goobers/goobers/internal/platform/lock"
	"github.com/goobers/goobers/internal/readmodel"
	"github.com/goobers/goobers/internal/telemetry"
)

const (
	attributionListPageSize        = 200
	faultAuditScanBudgetMultiplier = 10
	faultAuditStateSchema          = "goobers.dev/backprop/fault-audit-state/v1"
)

var (
	interventionEvidencePattern = regexp.MustCompile(`^intervention: stage (.+) attempt ([0-9]+) failed and attempt ([0-9]+) succeeded$`)
	faultAuditFindingIDPattern  = regexp.MustCompile(`^backprop-[0-9a-f]{20}$`)
)

// StoredAttributionQuery scopes the stored run evidence to aggregate.
type StoredAttributionQuery struct {
	Gaggle   string
	Workflow string
	Since    time.Time
	Until    time.Time
}

// StoredAttributionCohorts reads enrolled per-run attribution records and
// re-aggregates them by cohort.
func StoredAttributionCohorts(
	ctx context.Context,
	root string,
	reads readmodel.Reader,
	query StoredAttributionQuery,
) ([]creditgraph.CohortAggregation, error) {
	observations, err := storedAttributionObservations(ctx, root, reads, query)
	if err != nil {
		return nil, err
	}
	return creditgraph.AggregateAttributionEvidence(observations), nil
}

// StoredFaultAudit classifies enrolled terminal evidence without mutating
// workflows, issues, or runs.
func StoredFaultAudit(
	ctx context.Context,
	root string,
	reads readmodel.Reader,
	query StoredAttributionQuery,
	config creditgraph.FaultAuditConfig,
) (creditgraph.FaultAuditReport, error) {
	maxObservations := config.MaxObservations
	if maxObservations < 1 {
		maxObservations = 500
	}
	observations, err := storedAttributionObservationsLimit(ctx, root, reads, query, maxObservations)
	if err != nil {
		return creditgraph.FaultAuditReport{}, err
	}
	if config.Since.IsZero() {
		config.Since = query.Since
	}
	if config.Until.IsZero() {
		config.Until = query.Until
	}
	if config.Now.IsZero() {
		config.Now = time.Now().UTC()
	}
	state, err := readFaultAuditState(root)
	if err != nil {
		return creditgraph.FaultAuditReport{}, err
	}
	config.PreviousReports = mergeAuditTimes(previousReportsForScope(state.PreviousReports, query), config.PreviousReports)
	config.FixesAppliedAt = mergeAuditTimes(state.FixesAppliedAt, config.FixesAppliedAt)
	report := creditgraph.AuditFaultDomains(observations, config)
	if err := recordFaultAuditReports(ctx, root, query, config.Now, report); err != nil {
		return creditgraph.FaultAuditReport{}, err
	}
	return report, nil
}

func storedAttributionObservations(
	ctx context.Context,
	root string,
	reads readmodel.Reader,
	query StoredAttributionQuery,
) ([]creditgraph.AttributionObservation, error) {
	return storedAttributionObservationsLimit(ctx, root, reads, query, 0)
}

func storedAttributionObservationsLimit(
	ctx context.Context,
	root string,
	reads readmodel.Reader,
	query StoredAttributionQuery,
	limit int,
) ([]creditgraph.AttributionObservation, error) {
	if reads == nil || strings.TrimSpace(root) == "" {
		return nil, nil
	}
	layout := instance.NewLayout(root)
	options := readmodel.ListOptions{
		Gaggle: query.Gaggle, Workflow: query.Workflow,
		Since: query.Since, Until: query.Until, Limit: attributionListPageSize,
	}
	capacity := attributionListPageSize
	scanBudget := 0
	if limit > 0 {
		capacity = limit
		scanBudget = max(attributionListPageSize, limit*faultAuditScanBudgetMultiplier)
	}
	observations := make([]creditgraph.AttributionObservation, 0, capacity)
	terminalScanned := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := reads.ListRuns(ctx, options)
		if err != nil {
			return nil, fmt.Errorf("list attribution runs: %w", err)
		}
		for _, row := range page.Runs {
			if !row.Terminal {
				continue
			}
			if scanBudget > 0 && terminalScanned >= scanBudget {
				return observations, nil
			}
			terminalScanned++
			observation, ok, err := storedAttributionObservation(ctx, layout, row)
			if err != nil {
				return nil, err
			}
			if ok {
				observations = append(observations, observation)
				if limit > 0 && len(observations) == limit {
					return observations, nil
				}
			}
		}
		if !page.HasMore || page.Next.Zero() {
			return observations, nil
		}
		options.Cursor = page.Next
	}
}

type faultAuditState struct {
	Schema          string               `json:"schema"`
	PreviousReports map[string]time.Time `json:"previousReports,omitempty"`
	FixesAppliedAt  map[string]time.Time `json:"fixesAppliedAt,omitempty"`
}

func faultAuditStatePath(root string) string {
	return filepath.Join(instance.NewLayout(root).SchedulerDir(), "backprop-audit", "state.json")
}

func readFaultAuditState(root string) (faultAuditState, error) {
	state := faultAuditState{
		Schema: faultAuditStateSchema, PreviousReports: map[string]time.Time{}, FixesAppliedAt: map[string]time.Time{},
	}
	data, err := os.ReadFile(faultAuditStatePath(root))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return faultAuditState{}, fmt.Errorf("read fault audit state: %w", err)
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return faultAuditState{}, fmt.Errorf("decode fault audit state: %w", err)
	}
	if state.Schema != faultAuditStateSchema {
		return faultAuditState{}, fmt.Errorf("decode fault audit state: unsupported schema %q", state.Schema)
	}
	if state.PreviousReports == nil {
		state.PreviousReports = map[string]time.Time{}
	}
	if state.FixesAppliedAt == nil {
		state.FixesAppliedAt = map[string]time.Time{}
	}
	return state, nil
}

func mergeAuditTimes(stored, supplied map[string]time.Time) map[string]time.Time {
	merged := make(map[string]time.Time, len(stored)+len(supplied))
	for id, at := range stored {
		merged[id] = at
	}
	for id, at := range supplied {
		merged[id] = at
	}
	return merged
}

func previousReportsForScope(stored map[string]time.Time, query StoredAttributionQuery) map[string]time.Time {
	if strings.TrimSpace(query.Gaggle) == "" && strings.TrimSpace(query.Workflow) == "" {
		reports := map[string]time.Time{}
		for key, at := range stored {
			if strings.HasPrefix(key, "backprop-") {
				reports[key] = at
			}
		}
		return reports
	}
	prefix := faultAuditScopePrefix(query)
	reports := map[string]time.Time{}
	for key, at := range stored {
		if strings.HasPrefix(key, prefix) {
			reports[strings.TrimPrefix(key, prefix)] = at
		}
	}
	return reports
}

func faultAuditScopePrefix(query StoredAttributionQuery) string {
	scope := strings.TrimSpace(query.Gaggle) + "\x00" + strings.TrimSpace(query.Workflow)
	return fmt.Sprintf("scope-%x:", sha256.Sum256([]byte(scope)))
}

func faultAuditReportKey(query StoredAttributionQuery, findingID string) string {
	if strings.TrimSpace(query.Gaggle) == "" && strings.TrimSpace(query.Workflow) == "" {
		return findingID
	}
	return faultAuditScopePrefix(query) + findingID
}

func recordFaultAuditReports(ctx context.Context, root string, query StoredAttributionQuery, now time.Time, report creditgraph.FaultAuditReport) error {
	ids := make([]string, 0, len(report.ProductFindings)+len(report.ExternalFindings)+len(report.WorkflowFindings)+len(report.UnknownFindings))
	for _, findings := range [][]creditgraph.FaultFinding{report.ProductFindings, report.ExternalFindings, report.WorkflowFindings, report.UnknownFindings} {
		for _, finding := range findings {
			ids = append(ids, finding.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return updateFaultAuditState(ctx, root, func(state *faultAuditState) {
		for _, id := range ids {
			state.PreviousReports[faultAuditReportKey(query, id)] = now
		}
	})
}

// RecordFaultAuditFix marks a finding for held-out verification by subsequent
// report-only audit passes.
func RecordFaultAuditFix(ctx context.Context, root, findingID string, appliedAt time.Time) error {
	findingID = strings.TrimSpace(findingID)
	if !faultAuditFindingIDPattern.MatchString(findingID) || appliedAt.IsZero() {
		return fmt.Errorf("record fault audit fix: a valid Backprop finding ID and applied time are required")
	}
	state, err := readFaultAuditState(root)
	if err != nil {
		return err
	}
	known := false
	for key := range state.PreviousReports {
		if key == findingID || strings.HasSuffix(key, ":"+findingID) {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("record fault audit fix: finding %q has not been reported", findingID)
	}
	return updateFaultAuditState(ctx, root, func(state *faultAuditState) {
		state.FixesAppliedAt[findingID] = appliedAt
	})
}

func updateFaultAuditState(ctx context.Context, root string, update func(*faultAuditState)) error {
	path := faultAuditStatePath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create fault audit state directory: %w", err)
	}
	var held *platformlock.Handle
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		held, err = platformlock.TryAcquire(path + ".lock")
		if err == nil {
			break
		}
		if !errors.Is(err, platformlock.ErrHeld) {
			return fmt.Errorf("lock fault audit state: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer held.Release()
	state, err := readFaultAuditState(root)
	if err != nil {
		return err
	}
	update(&state)
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode fault audit state: %w", err)
	}
	data = append(data, '\n')
	if err := journal.WriteFileAtomic(path, data, 0o600); err != nil {
		return fmt.Errorf("write fault audit state: %w", err)
	}
	return nil
}

func storedAttributionObservation(
	ctx context.Context,
	layout instance.Layout,
	row readmodel.RunRow,
) (creditgraph.AttributionObservation, bool, error) {
	runDir, err := layout.FindRunDir(row.RunID)
	if err != nil {
		return creditgraph.AttributionObservation{}, false, fmt.Errorf("open attribution run %q: %w", row.RunID, err)
	}
	record, err := creditgraph.ReadRunRecord(runDir)
	if errors.Is(err, os.ErrNotExist) {
		return creditgraph.AttributionObservation{}, false, nil
	}
	if err != nil {
		return creditgraph.AttributionObservation{}, false, fmt.Errorf("read attribution record %q: %w", row.RunID, err)
	}
	observation := creditgraph.AttributionObservation{
		RunID: record.RunID, Workflow: record.Workflow, EffectiveVersion: record.EffectiveVersion,
		Workload: record.Workload, Status: record.Status, Failure: record.Failure,
		Attribution: record.Attribution,
		Evidence:    append([]creditgraph.AttributionEvidenceLink(nil), record.Evidence...),
	}
	if row.FinishedAt != nil {
		observation.ObservedAt = *row.FinishedAt
	} else {
		observation.ObservedAt = row.StartedAt
	}
	if record.Status == creditgraph.RecordFailed {
		return observation, true, nil
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
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
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
	attribution := record.Attribution
	observation.Attribution = attribution
	for _, node := range graph.Nodes {
		if node.Kind == creditgraph.KindEnvironment && strings.TrimSpace(node.Label) != "" {
			observation.Environments = appendUniqueString(observation.Environments, node.Label)
		}
	}
	slices.Sort(observation.Environments)
	observation.Evidence = append(
		observation.Evidence,
		buildAttributionEvidence(layout.Root, runDir, records, graph, attribution)...,
	)
	return observation, true, nil
}

func appendUniqueString(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
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
