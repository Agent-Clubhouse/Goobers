package creditgraph

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"

	"github.com/goobers/goobers/internal/journal"
)

const (
	// VerificationSchemaVersion identifies a checker adapter's scoped result.
	VerificationSchemaVersion = "goobers.dev/backprop/verification/v1"
	// VerificationAnnotation is emitted by the deterministic executor, never
	// by the agent claim-file ingestion path.
	VerificationAnnotation = "backprop-verification"
	// MaxVerificationBytes bounds one checker report before decoding.
	MaxVerificationBytes = 16 << 10
	// MaxVerificationsPerRun bounds accepted and rejected reports together.
	MaxVerificationsPerRun = 128
)

// VerificationOutcome describes a scoped checker result, not overall correctness.
type VerificationOutcome string

const (
	// VerificationProven means the adapter reports the scoped property checked.
	VerificationProven VerificationOutcome = "proven"
	// VerificationCounterexample means the checker established a violation.
	VerificationCounterexample VerificationOutcome = "counterexample"
	// VerificationInconclusive makes no claim that the property holds or fails.
	VerificationInconclusive VerificationOutcome = "inconclusive"
)

// VerificationChecker pins the trusted checker and its versioned executable.
type VerificationChecker struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

// VerificationReport is emitted by a reviewed deterministic checker adapter.
// Context binds dependencies, execution semantics, bounds, and other assumptions
// not captured by Specification. Evidence names previously recorded artifacts.
type VerificationReport struct {
	Schema        string              `json:"schema"`
	Property      string              `json:"property"`
	Subject       string              `json:"subject"`
	Specification string              `json:"specification"`
	Context       string              `json:"context"`
	Checker       VerificationChecker `json:"checker"`
	Assumptions   []string            `json:"assumptions"`
	Outcome       VerificationOutcome `json:"outcome"`
	Evidence      []string            `json:"evidence,omitempty"`
	Reason        string              `json:"reason,omitempty"`
	// Repairs explicitly names an earlier counterexample record in this run.
	Repairs string `json:"repairs,omitempty"`
}

// VerificationRecord retains the journal identity and provenance of a report.
// Accepted means its recorded provenance resolves, not that Backprop checked a
// proof. RepairVerified is scoped to the new Subject, never the current checkout.
type VerificationRecord struct {
	ID                string   `json:"id"`
	Sequence          uint64   `json:"sequence"`
	CompletedSequence uint64   `json:"completedSequence"`
	Stage             string   `json:"stage"`
	Attempt           int      `json:"attempt"`
	Branch            int      `json:"branch,omitempty"`
	Source            string   `json:"source"`
	SubjectNodeID     string   `json:"subjectNodeId,omitempty"`
	Reasons           []string `json:"reasons,omitempty"`
	RepairVerified    bool     `json:"repairVerified,omitempty"`
	RepairReason      string   `json:"repairReason,omitempty"`
	VerificationReport
}

// DecodeVerificationReport rejects unknown fields, trailing data, invalid
// digests, and oversized text rather than silently weakening a checked scope.
func DecodeVerificationReport(data []byte) (VerificationReport, error) {
	if len(data) > MaxVerificationBytes {
		return VerificationReport{}, errors.New("verification report exceeds byte limit")
	}
	var report VerificationReport
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return VerificationReport{}, fmt.Errorf("decode verification report: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return VerificationReport{}, errors.New("verification report must contain one JSON object")
	}
	if err := validateVerificationReport(report); err != nil {
		return VerificationReport{}, err
	}
	return report, nil
}

func validateVerificationReport(report VerificationReport) error {
	if report.Schema != VerificationSchemaVersion {
		return errors.New("unsupported verification schema")
	}
	for _, ref := range []string{report.Property, report.Checker.Name, report.Checker.Version} {
		if ref == "" || !validClaimRef(ref) {
			return errors.New("property and checker identity must be bounded printable references")
		}
	}
	for _, digest := range []string{report.Subject, report.Specification, report.Context, report.Checker.Digest} {
		if !claimDigestPattern.MatchString(digest) {
			return errors.New("subject, specification, context, and checker require sha256 digests")
		}
	}
	switch report.Outcome {
	case VerificationProven, VerificationCounterexample, VerificationInconclusive:
	default:
		return errors.New("verification outcome must be proven, counterexample, or inconclusive")
	}
	if report.Assumptions == nil {
		return errors.New("verification assumptions must be an explicit array")
	}
	if len(report.Assumptions) > 16 || len(report.Evidence) > 8 {
		return errors.New("verification assumptions or evidence exceed count limit")
	}
	for _, text := range append(append([]string(nil), report.Assumptions...), report.Reason) {
		if len(text) > 1024 || SanitizeClaimText(text) != text {
			return errors.New("verification text must be bounded and sanitized")
		}
	}
	if report.Outcome != VerificationInconclusive && len(report.Evidence) == 0 {
		return errors.New("conclusive verification needs recorded evidence")
	}
	if report.Outcome == VerificationInconclusive && report.Reason == "" {
		return errors.New("inconclusive verification needs a reason")
	}
	for _, digest := range report.Evidence {
		if !claimDigestPattern.MatchString(digest) {
			return errors.New("verification evidence requires sha256 artifact digests")
		}
	}
	if report.Repairs != "" && (!validClaimRef(report.Repairs) || report.Outcome != VerificationProven) {
		return errors.New("only a proven result can reference a repair")
	}
	return nil
}

// VerificationAnnotationPayload binds a checked report to its captured stdout.
// Only deterministic executor code should publish this runner annotation.
func VerificationAnnotationPayload(report VerificationReport, source string) map[string]any {
	return map[string]any{
		"kind": VerificationAnnotation, "executor": "deterministic",
		"source": source, "report": report,
	}
}

func verificationStageKey(event journal.Event) string {
	return strconv.Itoa(event.Branch) + ":" + stageAttemptKey(event.Stage, event.Attempt)
}

func (b *builder) addVerifications(events []journal.Event) {
	started := map[string]uint64{}
	artifacts := map[string][]journal.Event{}
	accepted := map[string]VerificationRecord{}
	evaluated := 0
	for index, event := range events {
		key := verificationStageKey(event)
		switch event.Type {
		case journal.EventStageStarted:
			started[key] = event.Seq
		case journal.EventStageFinished:
			delete(started, key)
		case journal.EventArtifactRecorded:
			if event.Ref != nil {
				artifacts[event.Ref.Digest] = append(artifacts[event.Ref.Digest], event)
			}
		case journal.EventRunnerAnnotation:
			if event.Runner["kind"] != VerificationAnnotation {
				continue
			}
			if evaluated >= MaxVerificationsPerRun {
				b.graph.DroppedVerifications++
				continue
			}
			evaluated++
			record, err := decodeVerificationAnnotation(event)
			if err != nil {
				b.graph.DroppedVerifications++
				continue
			}
			if event.Stage == "" || event.Attempt < 1 || event.Seq == 0 || started[key] == 0 {
				record.Reasons = append(record.Reasons, "checker attempt has no preceding stage.started")
			}
			if event.Runner["executor"] != "deterministic" {
				record.Reasons = append(record.Reasons, "report was not published by the deterministic executor")
			}
			record.CompletedSequence = verificationCompleted(events, index)
			if record.CompletedSequence == 0 {
				record.Reasons = append(record.Reasons, "checker attempt has no subsequent successful stage.finished")
			}
			if !verificationArtifact(artifacts[record.Source], event, started[key], true) {
				record.Reasons = append(record.Reasons, "report source was not recorded by this checker attempt")
			}
			for _, digest := range append([]string{record.Subject, record.Specification, record.Context}, record.Evidence...) {
				if !verificationArtifact(artifacts[digest], event, 0, false) {
					record.Reasons = append(record.Reasons, "bound artifact was not recorded before this report: "+digest)
				}
			}
			if len(record.Reasons) > 0 {
				b.graph.UnverifiedVerifications = append(b.graph.UnverifiedVerifications, record)
				continue
			}
			if record.Repairs != "" {
				record.RepairVerified, record.RepairReason = verificationRepair(record, accepted)
			}
			record.SubjectNodeID = "evidence:" + record.Subject
			accepted[record.ID] = record
			b.graph.Verifications = append(b.graph.Verifications, record)
		}
	}
}

func verificationCompleted(events []journal.Event, index int) uint64 {
	writer := verificationStageKey(events[index])
	for i := index - 1; i >= 0; i-- {
		if verificationStageKey(events[i]) != writer {
			continue
		}
		if events[i].Type == journal.EventAgentLifecycle {
			return 0
		}
		if events[i].Type == journal.EventStageStarted {
			break
		}
	}
	for _, event := range events[index+1:] {
		if verificationStageKey(event) != writer {
			continue
		}
		switch event.Type {
		case journal.EventStageStarted, journal.EventAgentLifecycle:
			return 0
		case journal.EventStageFinished:
			if event.Status == "success" && event.Seq > events[index].Seq {
				return event.Seq
			}
			return 0
		}
	}
	return 0
}

func decodeVerificationAnnotation(event journal.Event) (VerificationRecord, error) {
	data, err := json.Marshal(event.Runner["report"])
	if err != nil {
		return VerificationRecord{}, err
	}
	report, err := DecodeVerificationReport(data)
	if err != nil {
		return VerificationRecord{}, err
	}
	source, _ := event.Runner["source"].(string)
	if !claimDigestPattern.MatchString(source) {
		return VerificationRecord{}, errors.New("verification source requires an artifact digest")
	}
	return VerificationRecord{
		ID:       "verification:" + strconv.FormatUint(event.Seq, 10),
		Sequence: event.Seq, Stage: event.Stage, Attempt: event.Attempt,
		Branch: event.Branch, Source: source, VerificationReport: report,
	}, nil
}

func verificationArtifact(candidates []journal.Event, checker journal.Event, after uint64, source bool) bool {
	for _, candidate := range candidates {
		if candidate.Seq <= after || candidate.Seq >= checker.Seq {
			continue
		}
		if source {
			// The ordinary executor ArtifactRecorder is stage-neutral. Its
			// stdout name and position inside this live attempt bind the source;
			// explicitly scoped recorders must match all stage fields instead.
			scoped := candidate.Stage == checker.Stage && candidate.Attempt == checker.Attempt
			named := candidate.Stage == "" && candidate.Attempt == 0 && candidate.Name == checker.Stage+"/stdout.log"
			if (scoped || named) && candidate.Branch == checker.Branch {
				return true
			}
		} else if candidate.Branch == 0 || candidate.Branch == checker.Branch {
			return true
		}
	}
	return false
}

func verificationRepair(record VerificationRecord, previous map[string]VerificationRecord) (bool, string) {
	before, ok := previous[record.Repairs]
	if !ok || before.Outcome != VerificationCounterexample || before.CompletedSequence >= record.Sequence {
		return false, "repair reference is not an earlier accepted counterexample in this run"
	}
	if before.Branch != record.Branch || before.Property != record.Property ||
		before.Specification != record.Specification || before.Context != record.Context ||
		before.Checker != record.Checker || !slices.Equal(before.Assumptions, record.Assumptions) {
		return false, "repair changed the property, specification, context, checker, branch, or assumptions"
	}
	if before.Subject == record.Subject {
		return false, "repair must check a changed subject"
	}
	return true, ""
}
