package creditgraph

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/telemetry"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const (
	// RecordSchemaVersion identifies the persisted per-run attribution shape.
	RecordSchemaVersion = "goobers.dev/backprop/record/v1"
	// RecordFileName is the immutable, run-scoped Backprop result.
	RecordFileName = "attribution.json"
)

// RecordStatus distinguishes a completed estimate from an honest refusal and
// from an isolated analysis failure.
type RecordStatus string

const (
	// RecordComplete means attribution completed with defensible evidence.
	RecordComplete RecordStatus = "complete"
	// RecordInsufficientEvidence means attribution refused to infer a result.
	RecordInsufficientEvidence RecordStatus = "insufficient-evidence"
	// RecordFailed means the isolated attribution analysis encountered an error.
	RecordFailed RecordStatus = "failed"
)

// RunRecord is the persisted result for one enrolled terminal run.
type RunRecord struct {
	Schema           string                    `json:"schema"`
	Status           RecordStatus              `json:"status"`
	ContractVersion  string                    `json:"contractVersion"`
	Mode             apiv1.BackpropMode        `json:"mode,omitempty"`
	RunID            string                    `json:"runId"`
	Gaggle           string                    `json:"gaggle"`
	Workflow         string                    `json:"workflow"`
	WorkflowVersion  int                       `json:"workflowVersion"`
	WorkflowDigest   string                    `json:"workflowDigest"`
	EffectiveVersion string                    `json:"effectiveVersion"`
	Workload         string                    `json:"workload"`
	Attribution      Attribution               `json:"attribution"`
	Evidence         []AttributionEvidenceLink `json:"evidence,omitempty"`
	Failure          string                    `json:"failure,omitempty"`
	// StageComposition reports how a stage-granular run's record was composed
	// from its stage checkpoints. Absent for run-granular records.
	StageComposition *StageComposition `json:"stageComposition,omitempty"`
}

// StageComposition describes the stage checkpoints an end-of-run record
// composed. Checkpoints is the number of verified stage records whose journal
// segments were composed, in order, ahead of the uncheckpointed tail.
// Error discloses why composition was refused; the record is then computed
// from the whole journal and Checkpoints is zero.
type StageComposition struct {
	Checkpoints     int    `json:"checkpoints"`
	ThroughSequence uint64 `json:"throughSequence,omitempty"`
	Error           string `json:"error,omitempty"`
}

type pinnedDefinition struct {
	DSLVersion string `json:"dslVersion"`
	Spec       struct {
		Backprop *apiv1.BackpropConfig `json:"backprop,omitempty"`
	} `json:"Spec"`
}

// WriteRunRecord computes and atomically publishes attribution for an actively
// enrolled run. Shadow runs are skipped before any analysis: their attribution
// is computed on demand from the persisted journal by ObserveShadowRun, so
// shadow mode adds no work to terminalization. Local terminalization passes nil after run.finished is durable; terminal
// remains available to callers analyzing an immutable synthetic event stream.
// A stage-granular run's record composes its verified stage checkpoints.
func WriteRunRecord(runDir string, terminal *journal.Event) (bool, error) {
	// Stage-granular runs compose and publish under the checkpoint publication
	// lock, so no checkpoint lands between composition and this write.
	if run, _, enrolled, err := loadRunJournal(runDir, terminal); err == nil && enrolled &&
		run.config.Granularity == apiv1.BackpropGranularityStage {
		handle, lockErr := lockStagePublication(runDir, runRecordLockWait)
		if lockErr != nil {
			return true, fmt.Errorf("lock attribution record: %w", lockErr)
		}
		defer func() { _ = handle.Release() }()
	}
	record, enrolled, err := AnalyzeRun(runDir, terminal)
	if !enrolled {
		return false, err
	}
	if err != nil {
		record.Status = RecordFailed
		record.Failure = err.Error()
	}
	data, marshalErr := json.MarshalIndent(record, "", "  ")
	if marshalErr != nil {
		return true, fmt.Errorf("encode attribution record: %w", marshalErr)
	}
	data = append(data, '\n')
	if writeErr := journal.WriteFileAtomic(filepath.Join(runDir, RecordFileName), data, 0o600); writeErr != nil {
		return true, fmt.Errorf("write attribution record: %w", writeErr)
	}
	return true, err
}

// AnalyzeRun computes one record without changing the run. A stage-granular
// run's record composes its verified stage checkpoints with the journal tail
// they do not yet cover; the result equals the whole-journal analysis.
func AnalyzeRun(runDir string, terminal *journal.Event) (RunRecord, bool, error) {
	return analyzeRun(runDir, terminal, true)
}

// loadedRun is one enrolled run's journal, read once for analysis.
type loadedRun struct {
	reader   *journal.Reader
	identity journal.RunIdentity
	config   apiv1.BackpropConfig
	events   []journal.Event
	spanData map[string][]byte
}

func analyzeRun(runDir string, terminal *journal.Event, compose bool) (RunRecord, bool, error) {
	run, record, enrolled, err := loadRun(runDir, terminal)
	if err != nil || !enrolled {
		return record, enrolled, err
	}
	events := run.events
	if compose && run.config.Granularity == apiv1.BackpropGranularityStage {
		var composition StageComposition
		events, composition = composeStageEvents(runDir, run)
		record.StageComposition = &composition
	}
	return analyzeEvents(run, events, record)
}

func loadRun(runDir string, terminal *journal.Event) (loadedRun, RunRecord, bool, error) {
	return loadRunFor(runDir, terminal, nil, true)
}

// loadRunFor is loadRun for a run resolved under an optional gaggle override.
// activeOnly reports shadow runs as not enrolled before their events are read.
func loadRunFor(
	runDir string,
	terminal *journal.Event,
	override *apiv1.GaggleBackprop,
	activeOnly bool,
) (loadedRun, RunRecord, bool, error) {
	run, record, enrolled, err := loadRunJournalFor(runDir, terminal, override, activeOnly)
	if err != nil || !enrolled {
		return run, record, enrolled, err
	}
	run.spanData, err = readSpanData(run.reader, run.events)
	if err != nil {
		return run, record, true, err
	}
	return run, record, true, nil
}

// loadRunJournal is loadRun without span content, for callers that only need
// the journal's events. Only actively enrolled runs are enrolled: shadow runs
// write no stage checkpoints and no attribution record.
func loadRunJournal(runDir string, terminal *journal.Event) (loadedRun, RunRecord, bool, error) {
	return loadRunJournalFor(runDir, terminal, nil, true)
}

func loadRunJournalFor(
	runDir string,
	terminal *journal.Event,
	override *apiv1.GaggleBackprop,
	activeOnly bool,
) (loadedRun, RunRecord, bool, error) {
	reader, err := journal.OpenReadOnly(runDir)
	if err != nil {
		return loadedRun{}, RunRecord{}, false, fmt.Errorf("open run: %w", err)
	}
	identity, err := reader.Identity()
	if err != nil {
		return loadedRun{}, RunRecord{}, false, fmt.Errorf("read identity: %w", err)
	}
	config, enrolled, err := resolvedBackprop(reader, identity, override)
	if err != nil || !enrolled {
		return loadedRun{}, RunRecord{}, enrolled, err
	}
	if activeOnly && config.Mode != apiv1.BackpropModeActive {
		return loadedRun{}, RunRecord{}, false, nil
	}
	run := loadedRun{reader: reader, identity: identity, config: config}
	record := RunRecord{
		Schema: RecordSchemaVersion, Status: RecordComplete, ContractVersion: config.Version,
		Mode:  config.EffectiveMode(),
		RunID: identity.RunID, Gaggle: identity.Gaggle, Workflow: identity.Workflow,
		WorkflowVersion: identity.WorkflowVersion, WorkflowDigest: identity.WorkflowDigest,
		EffectiveVersion: (rollup.EffectiveVersion{
			WorkflowDigest: identity.WorkflowDigest,
			GooberDigest:   identity.GooberDigest,
		}).Hash(),
		Workload: string(identity.Trigger.Kind),
	}
	events, err := reader.Events()
	if err != nil {
		return run, record, true, fmt.Errorf("read events: %w", err)
	}
	if terminal != nil {
		terminalEvent := *terminal
		if terminalEvent.Seq == 0 && len(events) > 0 {
			terminalEvent.Seq = events[len(events)-1].Seq + 1
		}
		events = append(events, terminalEvent)
	}
	run.events = events
	return run, record, true, nil
}

func readSpanData(reader *journal.Reader, events []journal.Event) (map[string][]byte, error) {
	spanData := map[string][]byte{}
	for _, event := range events {
		if event.Type != journal.EventSpanRecorded || event.Ref == nil {
			continue
		}
		data, readErr := reader.SpanBytes(*event.Ref)
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("read span %s: %w", event.Ref.Digest, readErr)
		}
		spanData[event.Ref.Digest] = data
	}
	return spanData, nil
}

func analyzeEvents(run loadedRun, events []journal.Event, record RunRecord) (RunRecord, bool, error) {
	record.EffectiveVersion = runEffectiveVersion(run.identity, events, run.spanData)
	graph, err := Build(Input{
		RunID: run.identity.RunID, Gaggle: run.identity.Gaggle, Workflow: run.identity.Workflow,
		Events: events, SpanData: run.spanData,
	})
	if err != nil {
		return record, true, fmt.Errorf("build credit graph: %w", err)
	}
	record.Attribution = Attribute(graph)
	record.Evidence = recordEvidence(events, graph, record.Attribution)
	if attributionInsufficient(graph, record.Attribution) {
		record.Status = RecordInsufficientEvidence
	}
	return record, true, nil
}

func runEffectiveVersion(identity journal.RunIdentity, events []journal.Event, spanData map[string][]byte) string {
	models := map[string]struct{}{}
	harnesses := map[string]struct{}{}
	for _, event := range events {
		if event.Type != journal.EventSpanRecorded || event.Ref == nil {
			continue
		}
		data, ok := spanData[event.Ref.Digest]
		if !ok {
			continue
		}
		switch event.DataSchema {
		case telemetry.SpanSchema:
			var span telemetry.SpanRecord
			if json.Unmarshal(data, &span) != nil {
				continue
			}
			if model := span.Attributes[telemetry.AttrModel]; model != "" {
				models[model] = struct{}{}
			}
			if harness := span.Attributes[telemetry.AttrHarnessVersion]; harness != "" {
				harnesses[harness] = struct{}{}
			}
		case telemetry.GenAIEventSchema:
			recordTranscriptModels(data, models)
		}
	}
	if len(models) > 1 || len(harnesses) > 1 {
		return ""
	}
	var model, harness string
	for value := range models {
		model = value
	}
	for value := range harnesses {
		harness = value
	}
	return (rollup.EffectiveVersion{
		WorkflowDigest: identity.WorkflowDigest,
		GooberDigest:   identity.GooberDigest,
		Model:          model,
		HarnessVersion: harness,
	}).Hash()
}

func recordTranscriptModels(data []byte, models map[string]struct{}) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), len(data)+1)
	for scanner.Scan() {
		var record genaiRecord
		if json.Unmarshal(bytes.TrimSpace(scanner.Bytes()), &record) == nil && record.Model != "" {
			models[record.Model] = struct{}{}
		}
	}
}

func recordEvidence(events []journal.Event, graph *Graph, attribution Attribution) []AttributionEvidenceLink {
	byStage := map[string]journal.Event{}
	byArtifact := map[string]journal.Event{}
	byAgent := map[string]journal.Event{}
	bySpan := map[string]journal.Event{}
	bySequence := map[uint64]journal.Event{}
	var terminal journal.Event
	for _, event := range events {
		bySequence[event.Seq] = event
		switch event.Type {
		case journal.EventRunFinished:
			terminal = event
		case journal.EventStageStarted, journal.EventStageFinished:
			byStage[stageAttemptKey(event.Stage, event.Attempt)] = event
		case journal.EventArtifactRecorded:
			if event.Ref != nil {
				byArtifact[event.Ref.Digest] = event
			}
		case journal.EventAgentLifecycle:
			if event.Agent != nil {
				byAgent[event.Agent.ID] = event
			}
		case journal.EventSpanRecorded:
			if event.Ref != nil {
				bySpan[event.Ref.Digest] = event
			}
		}
	}
	links := make([]AttributionEvidenceLink, 0, len(attribution.Contributions))
	for _, contribution := range attribution.Contributions {
		node, ok := graph.Node(contribution.NodeID)
		if !ok {
			continue
		}
		for _, event := range evidenceEvents(node, terminal, byStage, byArtifact, byAgent, bySpan, bySequence) {
			link := AttributionEvidenceLink{
				RunID: attribution.RunID, NodeID: node.ID, Stage: node.Stage,
				Detail: fmt.Sprintf("score=%.6f, uncertainty=%.6f", contribution.Score, contribution.Uncertainty),
				Source: "contribution", JournalSequence: int64(event.Seq), JournalPath: "events.jsonl",
			}
			if event.Ref != nil {
				link.ArtifactPath = event.Ref.Path
				link.ArtifactDigest = event.Ref.Digest
				link.ArtifactMediaType = event.Ref.MediaType
			}
			links = append(links, link)
		}
	}
	for _, cause := range attribution.Causes {
		node, ok := graph.Node(cause.NodeID)
		if !ok {
			continue
		}
		nodeEvents := evidenceEvents(node, terminal, byStage, byArtifact, byAgent, bySpan, bySequence)
		if len(nodeEvents) == 0 {
			continue
		}
		details := cause.Evidence
		if len(details) == 0 {
			details = []string{cause.Summary}
		}
		for _, detail := range details {
			for _, event := range nodeEvents {
				link := AttributionEvidenceLink{
					RunID: attribution.RunID, NodeID: node.ID, Stage: cause.Stage,
					Detail: detail, Source: string(cause.Class),
					JournalSequence: int64(event.Seq), JournalPath: "events.jsonl",
				}
				if event.Ref != nil {
					link.ArtifactPath = event.Ref.Path
					link.ArtifactDigest = event.Ref.Digest
					link.ArtifactMediaType = event.Ref.MediaType
				}
				links = append(links, link)
			}
		}
	}
	return links
}

func evidenceEvents(
	node Node,
	terminal journal.Event,
	byStage map[string]journal.Event,
	byArtifact map[string]journal.Event,
	byAgent map[string]journal.Event,
	bySpan map[string]journal.Event,
	bySequence map[uint64]journal.Event,
) []journal.Event {
	switch node.Kind {
	case KindOutcome, KindRun:
		if terminal.Type == journal.EventRunFinished {
			return []journal.Event{terminal}
		}
	case KindStage:
		if event, ok := byStage[stageAttemptKey(node.Stage, node.Attempt)]; ok {
			return []journal.Event{event}
		}
	case KindEvidence:
		if event, ok := byArtifact[node.Attributes["digest"]]; ok {
			return []journal.Event{event}
		}
	case KindSubagent:
		if event, ok := byAgent[node.Label]; ok {
			return []journal.Event{event}
		}
	case KindEvaluator:
		sequence, err := strconv.ParseUint(node.Attributes["journalSequence"], 10, 64)
		if err != nil {
			return nil
		}
		if event, ok := bySequence[sequence]; ok {
			return []journal.Event{event}
		}
	case KindTool:
		events := make([]journal.Event, 0, len(toolSpanDigests(node.Attributes)))
		for _, digest := range toolSpanDigests(node.Attributes) {
			if event, ok := bySpan[digest]; ok {
				events = append(events, event)
			}
		}
		return events
	case KindRuntime, KindEnvironment:
		if event, ok := bySpan[node.Attributes["spanDigest"]]; ok {
			return []journal.Event{event}
		}
	case KindModelInvocation, KindToolCall, KindToolResult:
		if digest := spanDigest(node.ID); digest != "" {
			if event, ok := bySpan[digest]; ok {
				return []journal.Event{event}
			}
		}
		if len(node.ID) > len("model:") && node.ID[:len("model:")] == "model:" {
			if event, ok := byAgent[node.ID[len("model:"):]]; ok {
				return []journal.Event{event}
			}
		}
	}
	return nil
}

func stageAttemptKey(stage string, attempt int) string {
	if attempt < 1 {
		attempt = 1
	}
	return fmt.Sprintf("%s#%d", stage, attempt)
}

func spanDigest(nodeID string) string {
	hash := -1
	at := -1
	for i := len(nodeID) - 1; i >= 0; i-- {
		if hash < 0 && nodeID[i] == '#' {
			hash = i
			continue
		}
		if hash >= 0 && nodeID[i] == '@' {
			at = i
			break
		}
	}
	if at < 0 || hash <= at+1 {
		return ""
	}
	return nodeID[at+1 : hash]
}

// resolvedBackprop applies apiv1.ResolveBackprop to the run's trusted
// pinned workflow and an optional gaggle default. The returned config's Mode
// is the resolved mode.
func resolvedBackprop(
	reader *journal.Reader,
	identity journal.RunIdentity,
	override *apiv1.GaggleBackprop,
) (apiv1.BackpropConfig, bool, error) {
	var ref *journal.InputRef
	for i := range identity.Inputs {
		if identity.Inputs[i].Name == journal.PinnedWorkflowDefinitionInputName {
			ref = &identity.Inputs[i]
			break
		}
	}
	if ref == nil {
		return apiv1.BackpropConfig{}, false, nil
	}
	if ref.Integrity != apiv1.IntegrityTrusted {
		return apiv1.BackpropConfig{}, false, nil
	}
	data, err := reader.ArtifactBytes(ref.Ref)
	if err != nil {
		return apiv1.BackpropConfig{}, false, fmt.Errorf("read pinned workflow definition: %w", err)
	}
	var definition pinnedDefinition
	if err := json.Unmarshal(data, &definition); err != nil {
		return apiv1.BackpropConfig{}, false, fmt.Errorf("decode pinned workflow definition: %w", err)
	}
	config := apiv1.ResolveBackprop(definition.Spec.Backprop, override)
	if config.Mode != apiv1.BackpropModeActive && config.Mode != apiv1.BackpropModeShadow {
		return apiv1.BackpropConfig{}, false, nil
	}
	return config, true, nil
}

// RunEnrolled reports whether a run's trusted pinned workflow resolves to
// active Backprop without performing attribution. Shadow runs are not
// enrolled for terminal-path attribution; ObserveShadowRun computes them on
// demand.
func RunEnrolled(runDir string) (bool, error) {
	mode, err := runBackpropMode(runDir, nil)
	return mode == apiv1.BackpropModeActive, err
}

func runBackpropMode(runDir string, override *apiv1.GaggleBackprop) (apiv1.BackpropMode, error) {
	reader, err := journal.OpenReadOnly(runDir)
	if err != nil {
		return apiv1.BackpropModeOff, fmt.Errorf("open run: %w", err)
	}
	identity, err := reader.Identity()
	if err != nil {
		return apiv1.BackpropModeOff, fmt.Errorf("read identity: %w", err)
	}
	config, enrolled, err := resolvedBackprop(reader, identity, override)
	if err != nil || !enrolled {
		return apiv1.BackpropModeOff, err
	}
	return config.Mode, nil
}
func attributionInsufficient(graph *Graph, attribution Attribution) bool {
	if graph == nil || len(graph.Gaps) > 0 || len(attribution.Contributions) == 0 {
		return true
	}
	for _, contribution := range attribution.Contributions {
		if contribution.Provenance == ProvenanceRecorded && contribution.Confidence > 0 {
			return false
		}
	}
	return true
}

// ShadowObservation is one terminal run observed in shadow mode.
type ShadowObservation struct {
	Record RunRecord
	// ViaOverride is true when the run resolves to shadow only through the
	// gaggle override rather than its own pinned workflow.
	ViaOverride bool
}

// ObserveShadowRun computes the shadow-mode record for one terminal run on
// demand from its persisted journal, in memory and off the run's terminal
// path; nothing is written. Runs resolve to shadow through their own pinned
// workflow (mode=shadow) or only through the gaggle override. Runs that
// resolve to active or off, or that already published an active record, are
// not shadow runs.
func ObserveShadowRun(runDir string, override *apiv1.GaggleBackprop) (ShadowObservation, bool, error) {
	if _, err := os.Stat(filepath.Join(runDir, RecordFileName)); err == nil {
		return ShadowObservation{}, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return ShadowObservation{}, false, err
	}
	run, record, enrolled, err := loadRunFor(runDir, nil, override, false)
	if !enrolled {
		return ShadowObservation{}, false, err
	}
	if record.Mode != apiv1.BackpropModeShadow {
		return ShadowObservation{}, false, nil
	}
	// Shadow runs write no stage checkpoints, so the whole journal is analyzed.
	if err == nil {
		record, _, err = analyzeEvents(run, run.events, record)
	}
	if err != nil {
		record.Status = RecordFailed
		record.Failure = err.Error()
	}
	ownMode, modeErr := runBackpropMode(runDir, nil)
	if modeErr != nil {
		return ShadowObservation{}, false, modeErr
	}
	return ShadowObservation{Record: record, ViaOverride: ownMode != apiv1.BackpropModeShadow}, true, nil
}

// ReadRunRecord reads a previously published active attribution result. It
// rejects a shadow-mode record so shadow attribution cannot reach filing or
// gates.
func ReadRunRecord(runDir string) (RunRecord, error) {
	data, err := os.ReadFile(filepath.Join(runDir, RecordFileName))
	if err != nil {
		return RunRecord{}, err
	}
	var record RunRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return RunRecord{}, fmt.Errorf("decode attribution record: %w", err)
	}
	if record.Schema != RecordSchemaVersion {
		return RunRecord{}, fmt.Errorf("unsupported attribution schema %q", record.Schema)
	}
	if record.Mode == apiv1.BackpropModeShadow {
		return RunRecord{}, fmt.Errorf("attribution record for run %q is a shadow record", record.RunID)
	}
	return record, nil
}
