package creditgraph

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"github.com/goobers/goobers/api/schemas"
	"github.com/goobers/goobers/internal/journal"
)

func TestVerificationReportsMatchPublishedSchema(t *testing.T) {
	raw, err := schemas.FS.ReadFile(schemas.BackpropVerification)
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft2020
	uri := schemas.BaseURI + schemas.BackpropVerification
	if err := compiler.AddResource(uri, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(uri)
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []VerificationOutcome{VerificationProven, VerificationCounterexample, VerificationInconclusive} {
		report := testVerificationReport()
		report.Outcome = outcome
		if outcome == VerificationInconclusive {
			report.Reason, report.Evidence = "budget exhausted", nil
		}
		var document any
		if err := json.Unmarshal(reportBytes(t, report), &document); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(document); err != nil {
			t.Fatalf("%s report violates published schema: %v", outcome, err)
		}
	}
}

func verificationDigest(char string) string { return "sha256:" + strings.Repeat(char, 64) }

func testVerificationReport() VerificationReport {
	return VerificationReport{
		Schema: VerificationSchemaVersion, Property: "roundtrip",
		Subject: verificationDigest("a"), Specification: verificationDigest("b"), Context: verificationDigest("c"),
		Checker:     VerificationChecker{Name: "checker", Version: "1", Digest: verificationDigest("d")},
		Assumptions: []string{"single-threaded", "inputs <= 64 bytes"},
		Outcome:     VerificationCounterexample, Evidence: []string{verificationDigest("e")},
	}
}

func reportBytes(t *testing.T, report VerificationReport) []byte {
	t.Helper()
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func verificationEvents(report VerificationReport) []journal.Event {
	events := []journal.Event{
		{Type: journal.EventRunStarted},
		{Type: journal.EventStageStarted, Stage: "prepare", Attempt: 1},
	}
	for _, digest := range []string{report.Subject, verificationDigest("f"), report.Specification, report.Context, report.Evidence[0]} {
		events = append(events, journal.Event{Type: journal.EventArtifactRecorded, Stage: "prepare", Attempt: 1, Ref: &journal.Ref{Digest: digest}})
	}
	events = append(events,
		journal.Event{Type: journal.EventStageFinished, Stage: "prepare", Attempt: 1, Status: "success"},
		journal.Event{Type: journal.EventStageStarted, Stage: "check", Attempt: 1},
		journal.Event{Type: journal.EventArtifactRecorded, Stage: "check", Attempt: 1, Name: "check/stdout.log", Ref: &journal.Ref{Digest: verificationDigest("1")}},
		journal.Event{Type: journal.EventRunnerAnnotation, Stage: "check", Attempt: 1, Runner: VerificationAnnotationPayload(report, verificationDigest("1"))},
		journal.Event{Type: journal.EventStageFinished, Stage: "check", Attempt: 1, Status: "success"},
	)
	for i := range events {
		events[i].Seq = uint64(i + 1)
	}
	return events
}

func verificationGraph(t *testing.T, events []journal.Event) *Graph {
	t.Helper()
	graph, err := Build(Input{RunID: "verification-run", Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func TestVerificationRejectsMalformedAndUnboundedReports(t *testing.T) {
	base := testVerificationReport()
	for _, test := range []struct {
		name string
		edit func(*VerificationReport)
	}{
		{"schema", func(r *VerificationReport) { r.Schema = "other" }},
		{"outcome", func(r *VerificationReport) { r.Outcome = "failed" }},
		{"digest", func(r *VerificationReport) { r.Subject = "main" }},
		{"checker", func(r *VerificationReport) { r.Checker.Version = "" }},
		{"missing evidence", func(r *VerificationReport) { r.Evidence = nil }},
		{"inconclusive reason", func(r *VerificationReport) { r.Outcome = VerificationInconclusive }},
		{"unchecked repair", func(r *VerificationReport) { r.Repairs = "verification:1" }},
		{"text controls", func(r *VerificationReport) { r.Reason = "hidden\u202etext" }},
		{"assumption bounds", func(r *VerificationReport) { r.Assumptions = make([]string, 17) }},
		{"evidence bounds", func(r *VerificationReport) { r.Evidence = make([]string, 9) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := base
			test.edit(&report)
			if _, err := DecodeVerificationReport(reportBytes(t, report)); err == nil {
				t.Fatal("invalid report accepted")
			}
		})
	}
	valid := string(reportBytes(t, base))
	for _, data := range []string{
		`null`, valid + `{}`, strings.Repeat(" ", MaxVerificationBytes+1),
		strings.TrimSuffix(valid, "}") + `,"admitted":true}`,
	} {
		if _, err := DecodeVerificationReport([]byte(data)); err == nil {
			t.Fatalf("invalid JSON contract accepted: %.100s", data)
		}
	}
}

func TestVerificationRequiresRecordedProvenance(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func([]journal.Event)
	}{
		{"missing start", func(e []journal.Event) { e[8].Type = journal.EventRunnerAnnotation }},
		{"not deterministic", func(e []journal.Event) { e[10].Runner["executor"] = "agent" }},
		{"foreign source", func(e []journal.Event) { e[9].Stage = "prepare" }},
		{"missing subject", func(e []journal.Event) { e[2].Ref = nil }},
		{"future evidence", func(e []journal.Event) { e[6].Seq = 99 }},
		{"other branch evidence", func(e []journal.Event) { e[6].Branch = 2 }},
		{"failed checker", func(e []journal.Event) { e[11].Status = "failure" }},
		{"unfinished checker", func(e []journal.Event) { e[11].Type = journal.EventRunnerAnnotation }},
		{"agent checker", func(e []journal.Event) {
			e[11] = agentEvent("checker-agent", "", "check", "model", journal.AgentStarted)
			e[11].Attempt, e[11].Seq = 1, 12
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := verificationEvents(testVerificationReport())
			test.edit(events)
			graph := verificationGraph(t, events)
			if len(graph.Verifications) != 0 || len(graph.UnverifiedVerifications) != 1 || len(graph.UnverifiedVerifications[0].Reasons) == 0 {
				t.Fatalf("untrusted evidence accepted or silently lost: %+v", graph)
			}
		})
	}
}

func TestVerificationRepairsAreExplicitAndScopeBound(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*VerificationReport)
		want bool
	}{
		{name: "matching scope", want: true},
		{name: "property", edit: func(r *VerificationReport) { r.Property = "other" }},
		{name: "specification", edit: func(r *VerificationReport) { r.Specification = verificationDigest("c") }},
		{name: "context", edit: func(r *VerificationReport) { r.Context = verificationDigest("b") }},
		{name: "checker", edit: func(r *VerificationReport) { r.Checker.Version = "2" }},
		{name: "assumptions", edit: func(r *VerificationReport) { r.Assumptions = []string{"weaker"} }},
		{name: "unchanged subject", edit: func(r *VerificationReport) { r.Subject = verificationDigest("a") }},
		{name: "missing counterexample", edit: func(r *VerificationReport) { r.Repairs = "verification:999" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			events := verificationEvents(testVerificationReport())
			after := testVerificationReport()
			after.Outcome, after.Subject, after.Repairs = VerificationProven, verificationDigest("f"), "verification:11"
			if test.edit != nil {
				test.edit(&after)
			}
			events = append(events,
				journal.Event{Seq: 13, Type: journal.EventStageStarted, Stage: "recheck", Attempt: 1},
				journal.Event{Seq: 14, Type: journal.EventArtifactRecorded, Stage: "recheck", Attempt: 1, Ref: &journal.Ref{Digest: verificationDigest("2")}},
				journal.Event{Seq: 15, Type: journal.EventRunnerAnnotation, Stage: "recheck", Attempt: 1, Runner: VerificationAnnotationPayload(after, verificationDigest("2"))},
				journal.Event{Seq: 16, Type: journal.EventStageFinished, Stage: "recheck", Attempt: 1, Status: "success"},
			)
			graph := verificationGraph(t, events)
			if len(graph.Verifications) != 2 || graph.Verifications[1].RepairVerified != test.want {
				t.Fatalf("repair evidence = %+v, unverified = %+v", graph.Verifications, graph.UnverifiedVerifications)
			}
			if !test.want && graph.Verifications[1].RepairReason == "" {
				t.Fatal("unverified repair had no explanation")
			}
			attribution := Attribute(graph)
			if len(attribution.Verifications) != 2 {
				t.Fatal("attribution lost verification evidence")
			}
			var baseline []journal.Event
			for _, event := range events {
				if event.Type != journal.EventRunnerAnnotation {
					baseline = append(baseline, event)
				}
			}
			without := Attribute(verificationGraph(t, baseline))
			if !reflect.DeepEqual(attribution.Contributions, without.Contributions) || !reflect.DeepEqual(attribution.Causes, without.Causes) {
				t.Fatal("verification evidence changed attribution")
			}
		})
	}
}

func TestVerificationReplayAndBounds(t *testing.T) {
	report := testVerificationReport()
	report.Outcome, report.Reason, report.Evidence = VerificationInconclusive, "checker budget exhausted", nil
	events := verificationEvents(testVerificationReport())
	events[10].Runner = VerificationAnnotationPayload(report, verificationDigest("1"))
	graph := verificationGraph(t, events)
	if len(graph.Verifications) != 1 || graph.Verifications[0].Outcome != VerificationInconclusive {
		t.Fatal("inconclusive result was discarded or changed")
	}
	data, _ := json.Marshal(events)
	var replay []journal.Event
	if err := json.Unmarshal(data, &replay); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(graph.Verifications, verificationGraph(t, replay).Verifications) {
		t.Fatal("replay changed verification evidence")
	}
	malformed := events[10]
	malformed.Runner = map[string]any{"kind": VerificationAnnotation, "report": map[string]any{"schema": "bogus"}}
	events = append(events, malformed)
	if got := verificationGraph(t, events).DroppedVerifications; got != 1 {
		t.Fatalf("dropped malformed reports = %d", got)
	}
	for i := 0; i < MaxVerificationsPerRun; i++ {
		events = append(events, malformed)
	}
	bounded := verificationGraph(t, events)
	if len(bounded.Verifications)+len(bounded.UnverifiedVerifications) > MaxVerificationsPerRun || bounded.DroppedVerifications != MaxVerificationsPerRun+1 {
		t.Fatal("verification report bounds were not enforced")
	}
}

func TestVerificationPersistsInRunAndStageRecords(t *testing.T) {
	run, runDir := recordTestRun(t, map[string]any{"enabled": true, "version": "v1", "granularity": "stage"})
	defer func() { _ = run.Close() }()
	events := verificationEvents(testVerificationReport())
	for _, event := range events[1:] {
		event.Seq = 0
		if err := run.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := WriteStageCheckpoints(runDir); err != nil {
		t.Fatal(err)
	}
	set, err := ReadStageRecords(runDir)
	if err != nil || len(set.Records) != 2 || len(set.Records[1].Verifications) != 1 {
		t.Fatalf("stage verification evidence = %+v, %v", set, err)
	}
	if _, err := WriteRunRecord(runDir, &journal.Event{Type: journal.EventRunFinished, Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	record, err := ReadRunRecord(runDir)
	if err != nil || len(record.Attribution.Verifications) != 1 || record.StageComposition.Checkpoints != 2 {
		t.Fatalf("persisted verification evidence = %+v, %v", record, err)
	}
}
