package investigation

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type fixtureReader map[string][]byte

func (r fixtureReader) ReadArtifact(_ context.Context, p apiv1.ArtifactPointer, _ int64) ([]byte, error) {
	return r[p.Digest], nil
}

func evidenceFixture() (Evidence, []apiv1.ContextPointer, fixtureReader) {
	reader := fixtureReader{}
	var pointers []apiv1.ContextPointer
	makePointer := func(stage, slot, name string) apiv1.ArtifactPointer {
		data := []byte(name)
		p := apiv1.ArtifactPointer{Path: "artifacts/" + name, Digest: apiv1.Digest(data), Size: int64(len(data)), MediaType: "text/plain", Integrity: apiv1.IntegrityDerived}
		reader[p.Digest] = data
		pointers = append(pointers, apiv1.ContextPointer{Name: stage + ".artifact[" + slot + "]", Artifact: &p})
		return p
	}
	harness := makePointer("reproduce", "1", "harness")
	baseline := makePointer("reproduce", "2", "baseline")
	report := makePointer("instrument", "1", "report")
	fix := makePointer("implement-fix", "1", "fix")
	result := makePointer("validate-reproduction", "1", "result")
	support := makePointer("instrument", "2", "support")
	digest := apiv1.Digest([]byte("config"))
	evidence := Evidence{SchemaVersion: SchemaVersion,
		Subject:      Subject{Item: apiv1.ExternalRef{Kind: "issue", URI: "https://example.com/issues/1", Description: "investigate"}, Repository: "owner/repo", BaseRevision: strings.Repeat("a", 40), FixRevision: strings.Repeat("b", 40)},
		Environment:  Environment{Platform: "linux/amd64", Dimensions: map[string]any{"workers": 4}, ConfigDigest: digest},
		Reproduction: Reproduction{Harness: harness, Baseline: baseline, SourceSnapshotDigest: digest, OracleDigest: digest, Symptom: "unexpected stall"},
		Diagnosis:    Diagnosis{Report: report, Confidence: "confirmed", Evidence: []EvidenceRef{{Kind: "log", Artifact: support, ProducerStage: "instrument", Description: "causal evidence", CaptureContext: map[string]any{"workers": 4}}}},
		Fix:          Fix{Report: fix, DiffDigest: digest},
		Validation:   Validation{Result: result, SourceSnapshotDigest: digest, HarnessDigest: harness.Digest, OracleDigest: digest, CompletedAttempts: 3, SymptomObservationsBefore: 1, Passed: true},
	}
	return evidence, pointers, reader
}

func TestPrepareVerifiedRedactedEvidence(t *testing.T) {
	evidence, pointers, reader := evidenceFixture()
	evidence.Environment.Dimensions["note"] = "private-test-credential"
	evidence.Environment.Dimensions["escaped"] = "private<test>&credential"
	scrubber := journal.NewRegistryScrubber()
	scrubber.Register([]byte("private-test-credential"))
	scrubber.Register([]byte("private<test>&credential"))
	data, err := prepareEvidence(context.Background(), evidence, pointers, reader, scrubber)
	if err != nil {
		t.Fatal(err)
	}
	var got Evidence
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Environment.Dimensions["note"] != journal.Redacted || got.Environment.Dimensions["escaped"] != journal.Redacted || len(got.Attachments) != 0 {
		t.Fatalf("bad redaction or placeholder attachments: %+v", got)
	}
}

func TestPrepareRejectsInvalidEvidenceBeforePublication(t *testing.T) {
	for name, mutate := range map[string]func(*Evidence, []apiv1.ContextPointer, fixtureReader){
		"double slash host path": func(e *Evidence, _ []apiv1.ContextPointer, _ fixtureReader) {
			e.Environment.Dimensions["root"] = "//server/private"
		},
		"absolute host path": func(e *Evidence, _ []apiv1.ContextPointer, _ fixtureReader) {
			e.Environment.Dimensions["root"] = "/Users/operator/source"
		},
		"windows host path": func(e *Evidence, _ []apiv1.ContextPointer, _ fixtureReader) {
			e.Diagnosis.Evidence[0].Description = `captured in C:\Users\operator\source`
		},
		"credential-bearing URI": func(e *Evidence, _ []apiv1.ContextPointer, _ fixtureReader) {
			e.Subject.Item.URI = "https://user:password@example.com/issues/1"
		},
		"unsupported version": func(e *Evidence, _ []apiv1.ContextPointer, _ fixtureReader) { e.SchemaVersion = "future" },
		"no cause evidence":   func(e *Evidence, _ []apiv1.ContextPointer, _ fixtureReader) { e.Diagnosis.Evidence = nil },
		"unconfirmed":         func(e *Evidence, _ []apiv1.ContextPointer, _ fixtureReader) { e.Diagnosis.Confidence = "possible" },
		"symptom persists": func(e *Evidence, _ []apiv1.ContextPointer, _ fixtureReader) {
			e.Validation.SymptomObservationsAfter = 1
		},
		"unknown attachment kind": func(e *Evidence, _ []apiv1.ContextPointer, _ fixtureReader) { e.Diagnosis.Evidence[0].Kind = "unknown" },
		"wrong producer": func(e *Evidence, _ []apiv1.ContextPointer, _ fixtureReader) {
			e.Diagnosis.Evidence[0].ProducerStage = "reproduce"
		},
		"tampered bytes": func(e *Evidence, _ []apiv1.ContextPointer, r fixtureReader) {
			r[e.Fix.Report.Digest] = []byte("tampered")
		},
		"cross run":         func(_ *Evidence, p []apiv1.ContextPointer, _ fixtureReader) { p[0].RunID = "foreign" },
		"untrusted pointer": func(e *Evidence, _ []apiv1.ContextPointer, _ fixtureReader) { e.Fix.Report.Path = "artifacts/forged" },
		"non scalar dimension": func(e *Evidence, _ []apiv1.ContextPointer, _ fixtureReader) {
			e.Environment.Dimensions["nested"] = map[string]any{"raw": "value"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			evidence, pointers, reader := evidenceFixture()
			mutate(&evidence, pointers, reader)
			data, err := prepareEvidence(context.Background(), evidence, pointers, reader, journal.NewRegistryScrubber())
			if err == nil || data != nil {
				t.Fatalf("invalid evidence prepared for publication: %v", err)
			}
		})
	}
}
