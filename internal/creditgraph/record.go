package creditgraph

import (
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
	RecordComplete             RecordStatus = "complete"
	RecordInsufficientEvidence RecordStatus = "insufficient-evidence"
	RecordFailed               RecordStatus = "failed"
)

// RunRecord is the persisted result for one enrolled terminal run.
type RunRecord struct {
	Schema           string                    `json:"schema"`
	Status           RecordStatus              `json:"status"`
	ContractVersion  string                    `json:"contractVersion"`
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
}

type pinnedDefinition struct {
	DSLVersion string `json:"dslVersion"`
	Spec       struct {
		Backprop *apiv1.BackpropConfig `json:"backprop,omitempty"`
	} `json:"Spec"`
}

// WriteRunRecord computes and atomically publishes attribution for an enrolled
// run. Local terminalization passes nil after run.finished is durable; terminal
// remains available to callers analyzing an immutable synthetic event stream.
func WriteRunRecord(runDir string, terminal *journal.Event) (bool, error) {
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

// AnalyzeRun computes one record without changing the run.
func AnalyzeRun(runDir string, terminal *journal.Event) (RunRecord, bool, error) {
	reader, err := journal.OpenReadOnly(runDir)
	if err != nil {
		return RunRecord{}, false, fmt.Errorf("open run: %w", err)
	}
	identity, err := reader.Identity()
	if err != nil {
		return RunRecord{}, false, fmt.Errorf("read identity: %w", err)
	}
	config, enrolled, err := enrolledBackprop(reader, identity)
	if err != nil || !enrolled {
		return RunRecord{}, enrolled, err
	}
	record := RunRecord{
		Schema: RecordSchemaVersion, Status: RecordComplete, ContractVersion: config.Version,
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
		return record, true, fmt.Errorf("read events: %w", err)
	}
	if terminal != nil {
		terminalEvent := *terminal
		if terminalEvent.Seq == 0 && len(events) > 0 {
			terminalEvent.Seq = events[len(events)-1].Seq + 1
		}
		events = append(events, terminalEvent)
	}
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
			return record, true, fmt.Errorf("read span %s: %w", event.Ref.Digest, readErr)
		}
		spanData[event.Ref.Digest] = data
	}
	record.EffectiveVersion = runEffectiveVersion(identity, spanData)
	graph, err := Build(Input{
		RunID: identity.RunID, Gaggle: identity.Gaggle, Workflow: identity.Workflow,
		Events: events, SpanData: spanData,
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

func runEffectiveVersion(identity journal.RunIdentity, spanData map[string][]byte) string {
	type modelHarness struct {
		model   string
		harness string
	}
	versions := map[modelHarness]struct{}{}
	for _, data := range spanData {
		var span telemetry.SpanRecord
		if json.Unmarshal(data, &span) != nil {
			continue
		}
		model, hasModel := span.Attributes[telemetry.AttrModel]
		harness, hasHarness := span.Attributes[telemetry.AttrHarnessVersion]
		if hasModel || hasHarness {
			versions[modelHarness{model: model, harness: harness}] = struct{}{}
		}
	}
	var model, harness string
	if len(versions) == 1 {
		for version := range versions {
			model, harness = version.model, version.harness
		}
	}
	if len(versions) > 1 {
		return ""
	}
	return (rollup.EffectiveVersion{
		WorkflowDigest: identity.WorkflowDigest,
		GooberDigest:   identity.GooberDigest,
		Model:          model,
		HarnessVersion: harness,
	}).Hash()
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
		event, found := evidenceEvent(node, terminal, byStage, byArtifact, byAgent, bySpan, bySequence)
		if !found {
			continue
		}
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
	for _, cause := range attribution.Causes {
		node, ok := graph.Node(cause.NodeID)
		if !ok {
			continue
		}
		event, found := evidenceEvent(node, terminal, byStage, byArtifact, byAgent, bySpan, bySequence)
		if !found {
			continue
		}
		details := cause.Evidence
		if len(details) == 0 {
			details = []string{cause.Summary}
		}
		for _, detail := range details {
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
	return links
}

func evidenceEvent(
	node Node,
	terminal journal.Event,
	byStage map[string]journal.Event,
	byArtifact map[string]journal.Event,
	byAgent map[string]journal.Event,
	bySpan map[string]journal.Event,
	bySequence map[uint64]journal.Event,
) (journal.Event, bool) {
	switch node.Kind {
	case KindOutcome, KindRun:
		return terminal, terminal.Type == journal.EventRunFinished
	case KindStage:
		event, ok := byStage[stageAttemptKey(node.Stage, node.Attempt)]
		return event, ok
	case KindEvidence:
		event, ok := byArtifact[node.Attributes["digest"]]
		return event, ok
	case KindSubagent:
		event, ok := byAgent[node.Label]
		return event, ok
	case KindEvaluator:
		sequence, err := strconv.ParseUint(node.Attributes["journalSequence"], 10, 64)
		if err != nil {
			return journal.Event{}, false
		}
		event, ok := bySequence[sequence]
		return event, ok
	case KindTool, KindRuntime, KindEnvironment:
		event, ok := bySpan[node.Attributes["spanDigest"]]
		return event, ok
	case KindModelInvocation, KindToolCall, KindToolResult:
		if digest := spanDigest(node.ID); digest != "" {
			event, ok := bySpan[digest]
			return event, ok
		}
		if len(node.ID) > len("model:") && node.ID[:len("model:")] == "model:" {
			event, ok := byAgent[node.ID[len("model:"):]]
			return event, ok
		}
	}
	return journal.Event{}, false
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

func enrolledBackprop(reader *journal.Reader, identity journal.RunIdentity) (apiv1.BackpropConfig, bool, error) {
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
	if definition.Spec.Backprop == nil || !definition.Spec.Backprop.Enabled {
		return apiv1.BackpropConfig{}, false, nil
	}
	return *definition.Spec.Backprop, true, nil
}

// RunEnrolled reports whether a run's trusted pinned workflow opted into
// Backprop without performing attribution.
func RunEnrolled(runDir string) (bool, error) {
	reader, err := journal.OpenReadOnly(runDir)
	if err != nil {
		return false, fmt.Errorf("open run: %w", err)
	}
	identity, err := reader.Identity()
	if err != nil {
		return false, fmt.Errorf("read identity: %w", err)
	}
	_, enrolled, err := enrolledBackprop(reader, identity)
	return enrolled, err
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

// ReadRunRecord reads a previously published attribution result.
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
	return record, nil
}
