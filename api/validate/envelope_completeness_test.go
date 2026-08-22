package validate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/api/schemas"
	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type schemaFixture struct {
	schema string
	value  any
}

func TestSchemaBackedEnvelopeCompleteness(t *testing.T) {
	fixtures := map[string]schemaFixture{
		"artifact": {
			schema: schemas.Envelope["artifact"],
			value:  completeArtifactPointer("artifacts/review/evidence.json"),
		},
		"invocation": {
			schema: schemas.Envelope["invocation"],
			value:  completeInvocationEnvelope(),
		},
		"result": {
			schema: schemas.Envelope["result"],
			value:  completeResultEnvelope(),
		},
		"verdict": {
			schema: schemas.Envelope["verdict"],
			value:  completeVerdict(),
		},
		"remediation-brief": {
			schema: schemas.RemediationBrief,
			value:  completeRemediationBrief(),
		},
		// journal-event is not in schemas.Envelope (it's schemas.Journal, a
		// distinct wire contract — ARCHITECTURE.md §4), but the same
		// producer/schema drift this guard exists to prevent applies to it
		// exactly as it does to the four Envelope kinds: #2042 was a real
		// instance of the #1700/#1704 class (DataSchema added to
		// journal.Event with no matching schema property) that this
		// completeness check did not yet cover.
		"journal-event": {
			schema: schemas.Journal["event"],
			value:  completeJournalEvent(),
		},
	}

	for name := range schemas.Envelope {
		if _, ok := fixtures[name]; !ok {
			t.Errorf("schema-backed envelope %q has no complete round-trip fixture", name)
		}
	}

	validator := newV(t)
	for name, fixture := range fixtures {
		t.Run(name, func(t *testing.T) {
			assertEveryJSONFieldPopulated(t, fixture.value)
			data, err := json.Marshal(fixture.value)
			if err != nil {
				t.Fatalf("marshal fully populated %s: %v", name, err)
			}
			if err := validator.ValidateJSON(fixture.schema, data); err != nil {
				t.Fatalf("fully populated %s does not match %s: %v\n%s", name, fixture.schema, err, data)
			}
		})
	}
}

func completeArtifactPointer(path string) apiv1.ArtifactPointer {
	return apiv1.ArtifactPointer{
		Path:      path,
		Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		MediaType: "application/json",
		Size:      42,
		Integrity: apiv1.IntegrityDerived,
	}
}

func completeInvocationEnvelope() apiv1.InvocationEnvelope {
	return apiv1.InvocationEnvelope{
		TaskID:              "implement",
		Attempt:             2,
		WorkflowID:          "implementation",
		RunID:               "run-123",
		TriggerRef:          "github:issue:1704",
		Gaggle:              "goobers",
		BranchNamespace:     "goobers/",
		BaseBranch:          "main",
		Goober:              "implementer",
		Goal:                "implement the claimed issue",
		InstructionAddendum: "Preserve the public contract.",
		Workspace:           "/workspace",
		RepoRef: apiv1.RepoRef{
			Provider:      apiv1.ProviderADO,
			BaseURL:       "https://gitea.example.com",
			Owner:         "agent-clubhouse",
			Project:       "goobers-project",
			Name:          "goobers",
			Branch:        "main",
			ConnectionRef: "origin",
		},
		AdditionalWorkspaces: []apiv1.AdditionalWorkspace{{
			Name: "reference",
			Path: "/workspace-reference",
		}},
		CheckoutCones: map[string][]string{
			"":          {"services/web"},
			"reference": {"docs"},
		},
		Item: &apiv1.BacklogItem{
			ID:        "1704",
			Provider:  apiv1.ProviderGitHub,
			Title:     "Keep schemas synchronized",
			Body:      "Add a structural drift guard.",
			URL:       "https://example.test/issues/1704",
			Labels:    []string{"type:bug"},
			Integrity: apiv1.IntegrityMaintainer,
		},
		ContextPointers: []apiv1.ContextPointer{
			{
				Name:       "evidence",
				Branch:     1,
				BranchName: "gather",
				Integrity:  apiv1.IntegrityDerived,
				Artifact:   pointer(completeArtifactPointer("artifacts/gather/evidence.json")),
				RunID:      "source-run",
			},
			{
				Name:      "issue",
				Integrity: apiv1.IntegrityUnapproved,
				External: &apiv1.ExternalRef{
					Kind:        "issue",
					URI:         "https://example.test/issues/1704",
					Description: "claimed issue",
				},
			},
		},
		MinimumIntegrity: apiv1.IntegrityMaintainer,
		Capabilities:     []string{"repo:push"},
		Limits: apiv1.Limits{
			MaxDurationSeconds: 600,
			MaxTokens:          10_000,
			MaxCostUSD:         1.5,
		},
		Inputs: map[string]interface{}{"repass": false},
	}
}

func completeResultEnvelope() apiv1.ResultEnvelope {
	artifact := completeArtifactPointer("artifacts/implement/result.json")
	return apiv1.ResultEnvelope{
		Status:     apiv1.ResultFailure,
		Outputs:    map[string]interface{}{"attempt": 1},
		Artifacts:  []apiv1.ArtifactPointer{artifact},
		Transcript: pointer(completeArtifactPointer("artifacts/implement/transcript.txt")),
		Summary:    "The implementation needs another pass.",
		Metrics:    map[string]float64{"duration_seconds": 2.5},
		Error: &apiv1.ErrorInfo{
			Code:      "RETRY",
			Message:   "a retryable failure",
			Retryable: true,
		},
		Integrity: apiv1.IntegrityDerived,
	}
}

func completeVerdict() apiv1.Verdict {
	return apiv1.Verdict{
		Decision:  apiv1.VerdictNeedsChanges,
		Rationale: "One substantive finding remains.",
		Evidence:  []apiv1.ArtifactPointer{completeArtifactPointer("artifacts/review/evidence.json")},
		Findings: []apiv1.Finding{{
			Severity:    apiv1.SeverityError,
			Message:     "Wait for the overlapping pull request.",
			Location:    "api/v1alpha1/envelope.go:311",
			Class:       apiv1.FindingCrossPRBlocked,
			BlockingPRs: []int{1703},
		}},
		Summary:        "Changes are required.",
		HeadSHA:        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA:        "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Digest:         "sha256:review-inputs",
		SourceRunID:    "review-run",
		OverlapCluster: true,
		Elected:        true,
	}
}

func completeRemediationBrief() apiv1.RemediationBrief {
	return apiv1.RemediationBrief{
		Schema:                 apiv1.RemediationBriefVersion,
		Integrity:              apiv1.IntegrityUnapproved,
		SelectedNumber:         "1704",
		Head:                   "goobers/implementation/run-1704",
		Base:                   "main",
		WorkspaceBranch:        "goobers/implementation/run-1704",
		IsBehindBase:           true,
		HasSubstantiveFindings: "true",
		HasFailingCI:           "true",
		GatherPRContext: apiv1.RemediationPRContext{
			HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Verdict: pointer(completeVerdict()),
			Comments: []apiv1.RemediationThreadComment{{
				Author:    "reviewer",
				Body:      "Address every finding.",
				CreatedAt: "2026-07-26T12:00:00Z",
				URL:       "https://example.test/comments/1",
				Integrity: apiv1.IntegrityUnapproved,
			}},
		},
		GatherCIFailures: &apiv1.RemediationCIFailures{
			Checks: []apiv1.RemediationCIFailure{{
				Name:       "unit",
				Conclusion: "failure",
				URL:        "https://example.test/checks/1",
				Summary:    "A test failed.",
				Annotations: []apiv1.RemediationCIAnnotation{{
					Path:      "api/validate/validate_test.go",
					StartLine: 10,
					EndLine:   11,
					Level:     "failure",
					Title:     "schema drift",
					Message:   "The schema is missing a field.",
				}},
			}},
		},
		GatherReviewThreads: &apiv1.RemediationReviewThreads{
			Reviews: []apiv1.RemediationNativeReview{{
				Author:      "reviewer",
				State:       "changes_requested",
				Body:        "Update the schema.",
				CommitSHA:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				SubmittedAt: "2026-07-26T12:00:00Z",
				URL:         "https://example.test/reviews/1",
				Integrity:   apiv1.IntegrityUnapproved,
			}},
			InlineComments: []apiv1.RemediationInlineComment{{
				ID: 1, ThreadID: "PRRT_1",
				Author:            "reviewer",
				Body:              "This field is missing.",
				Path:              "api/schemas/verdict.schema.json",
				Line:              40,
				OriginalLine:      38,
				Side:              "RIGHT",
				StartLine:         39,
				OriginalStartLine: 37,
				StartSide:         "RIGHT",
				DiffHunk:          "@@ -37,2 +39,2 @@",
				InReplyTo:         1,
				IsResolved:        true,
				IsOutdated:        true,
				CreatedAt:         "2026-07-26T12:00:00Z",
				URL:               "https://example.test/comments/2",
				Integrity:         apiv1.IntegrityUnapproved,
			}},
		},
		GatherSiblingContext: &apiv1.RemediationSiblingContext{
			PullRequests: []apiv1.RemediationSibling{{
				Number:           1703,
				Head:             "fix/schema",
				HeadSHA:          "cccccccccccccccccccccccccccccccccccccccc",
				Blocking:         true,
				Reason:           "overlapping files",
				OverlappingFiles: []string{"api/schemas/verdict.schema.json"},
			}},
		},
		GatherIssueContext: &apiv1.RemediationIssueContext{
			Issues: []apiv1.RemediationIssue{{
				Number:    "1704",
				Title:     "Keep schemas synchronized",
				Body:      "Add a structural drift guard.",
				URL:       "https://example.test/issues/1704",
				Integrity: apiv1.IntegrityMaintainer,
			}},
		},
	}
}

// completeJournalEvent populates every exported journal.Event field at once —
// no single real event carries all of these together (e.g. Gate/Verdict and
// Stage/Attempt never co-occur), the same non-realistic-but-structurally-
// complete convention the other fixtures in this file already use. Its
// purpose is solely to prove every Go field has a schema counterpart (#2042):
// DataSchema is the field that fixture would have caught before it shipped.
func completeJournalEvent() journal.Event {
	return journal.Event{
		Schema:              "goobers.dev/journal/event/v1",
		Seq:                 1,
		Type:                journal.EventSpanRecorded,
		Branch:              1,
		Time:                time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Stage:               "impl",
		Attempt:             1,
		AttemptClass:        journal.AttemptPolicy,
		Actor:               "maintainer@example.com",
		Action:              "override",
		Decision:            "pass",
		Rationale:           "accepted risk",
		InstructionAddendum: "Reuse the existing parser.",
		Gate:                "review",
		Verdict:             "needs-changes",
		Target:              "implement",
		Complete:            true,
		Escalated:           true,
		Status:              "success",
		WorkflowVersion:     1,
		WorkflowDigest:      "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Outputs:             map[string]any{"ciStatus": "success"},
		Artifacts: []journal.Ref{{
			Path:      "artifacts/sha256/aa/plan.txt",
			Digest:    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Size:      10,
			MediaType: "text/plain",
			Integrity: apiv1.IntegrityTrusted,
		}},
		Integrity:        apiv1.IntegrityTrusted,
		MinimumIntegrity: apiv1.IntegrityMaintainer,
		Ref: &journal.Ref{
			Path:      "spans/sha256/bb/transcript.json",
			Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Size:      20,
			MediaType: "application/json",
			Integrity: apiv1.IntegrityDerived,
		},
		Name: "transcript",
		// The field #2042 was filed over: a span.recorded event's schema
		// identifier, populated on essentially every agentic run
		// (internal/harness/executor.go defaults TranscriptSchema to
		// telemetry.GenAIEventSchema whenever the adapter leaves it empty).
		DataSchema:  "goobers.dev/telemetry/genai-event/v1",
		ExternalRef: &journal.ExternalRef{Provider: "github", Kind: "pr", ID: "42", URL: "https://example.test/pr/42"},
		Error:       &journal.ErrorDetail{Code: "boom", Message: "detail"},
		Redaction: &journal.RedactionInfo{
			Target:    "artifacts/sha256/cc/leak.txt",
			OldDigest: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			NewDigest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
			Reason:    "secret detected",
		},
		Runner: map[string]any{"posture": "enforced"},
		NotificationRequest: pointer(apiv1.NotificationRequest{
			Schema: apiv1.NotificationRequestSchema, NotificationID: "notice-1",
			IncidentID: "incident-1", EventID: "event-1",
			Severity: apiv1.NotificationSeverityCritical, Transition: "opened",
			Title: "Build failed", Body: "The release build failed.", SpeechText: "Release build failed.",
			Facts: []apiv1.NotificationFact{{Name: "branch", Value: "main"}},
			Evidence: []apiv1.NotificationEvidenceRef{{
				Kind: "artifact", ID: "build-log",
				Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}},
			Source: apiv1.NotificationSource{RunID: "run-123", Workflow: "mission-control", Stage: "decide"},
			Sinks:  []string{"terminal"}, ExpiresAt: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC),
			IdempotencyKey: "incident-1:opened",
		}),
		NotificationReceipt: pointer(apiv1.NotificationReceipt{
			Schema: apiv1.NotificationReceiptSchema, NotificationID: "notice-1",
			IdempotencyKey:    "incident-1:opened",
			IdempotencyDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Source:            apiv1.NotificationSource{RunID: "run-123", Workflow: "mission-control", Stage: "decide"},
			Evidence: []apiv1.NotificationEvidenceRef{{
				Kind: "artifact", ID: "build-log",
				Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}},
			Sink:    apiv1.NotificationSinkRef{Kind: "terminal", Version: "v1"},
			Attempt: 1, StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			CompletedAt: time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
			Status:      apiv1.NotificationDelivered, Unresolved: true,
			ExternalReference: "delivery-1", Error: "none",
		}),
		Agent: &journal.AgentProvenance{
			Schema: "goobers.dev/journal/agent/v1", ID: "worker", ParentID: "coordinator",
			RunID: "run-123", Stage: "implement", Attempt: 2, Plugin: "copilot",
			Objective: "implement the issue", Coordinator: true, Worker: true, Leaf: true,
			RequestedModel: "requested-model", ResolvedModel: "resolved-model",
			RequestedReasoningEffort: "high", ResolvedReasoningEffort: "medium",
			Lifecycle: journal.AgentCompleted,
			StartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC),
			Budget: journal.AgentUsage{
				InputTokens: pointer(int64(100)), OutputTokens: pointer(int64(50)), CostUSD: pointer(1.5),
			},
			Usage: journal.AgentUsage{
				InputTokens: pointer(int64(80)), OutputTokens: pointer(int64(40)), CostUSD: pointer(1.25),
			},
			UsageAggregated: true,
			Results: []journal.Ref{{
				Path: "artifacts/result", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Size: 42, MediaType: "application/json", Integrity: apiv1.IntegrityDerived,
			}},
			DependsOn: []string{"dependency"}, Fidelity: journal.AgentFidelityFull,
		},
		PeerMessage: &journal.PeerMessageMetadata{
			ID: "message-1", SenderID: "worker", RecipientID: "coordinator",
			OccurredAt: time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC), Purpose: "finding",
			Artifact: &journal.Ref{
				Path: "artifacts/message", Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				Size: 12, MediaType: "application/json", Integrity: apiv1.IntegrityDerived,
			},
			ContentHash: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		},
		Parallel:     "fanout",
		BranchName:   "east",
		BranchStatus: journal.BranchSucceeded,
		Completeness: []journal.BranchOutcome{{
			Branch: 1, Name: "east", Status: journal.BranchSucceeded, Artifacts: 1,
		}},
		Workflow:  "implementation",
		Gaggle:    "goobers",
		RunID:     "run-123",
		Reason:    "manual trigger",
		SkipCount: 1,
	}
}

func pointer[T any](value T) *T {
	return &value
}

var completenessOmissions = map[reflect.Type]map[string]string{
	reflect.TypeOf(apiv1.RepoRef{}): {
		"Checkout": "workspace materialization config is intentionally projected out by RepoRef.EnvelopeRef",
	},
}

func assertEveryJSONFieldPopulated(t *testing.T, value any) {
	t.Helper()
	var missing []string
	collectUnpopulatedJSONFields(
		[]reflect.Value{reflect.ValueOf(value)},
		reflect.TypeOf(value).Name(),
		&missing,
	)
	if len(missing) == 0 {
		return
	}
	sort.Strings(missing)
	t.Fatalf("fully populated fixture leaves exported JSON field(s) at zero:\n  %s", strings.Join(missing, "\n  "))
}

func collectUnpopulatedJSONFields(values []reflect.Value, path string, missing *[]string) {
	values = indirectValues(values)
	if len(values) == 0 {
		return
	}

	switch values[0].Kind() {
	case reflect.Struct:
		typ := values[0].Type()
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := reflectedJSONFieldName(field)
			if !field.IsExported() || name == "" || isCompletenessOmission(typ, field.Name) {
				continue
			}
			fieldValues := make([]reflect.Value, 0, len(values))
			for _, value := range values {
				candidate := value.Field(i)
				if isPopulatedJSONValue(candidate) {
					fieldValues = append(fieldValues, candidate)
				}
			}
			fieldPath := fmt.Sprintf("%s.%s", path, name)
			if len(fieldValues) == 0 {
				*missing = append(*missing, fieldPath)
				continue
			}
			collectUnpopulatedJSONFields(fieldValues, fieldPath, missing)
		}
	case reflect.Slice, reflect.Array:
		var elements []reflect.Value
		for _, value := range values {
			for i := 0; i < value.Len(); i++ {
				elements = append(elements, value.Index(i))
			}
		}
		collectUnpopulatedJSONFields(elements, path+"[]", missing)
	}
}

func isPopulatedJSONValue(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Map, reflect.Slice, reflect.String:
		return value.Len() > 0
	default:
		return !value.IsZero()
	}
}

func indirectValues(values []reflect.Value) []reflect.Value {
	indirect := make([]reflect.Value, 0, len(values))
	for _, value := range values {
		for value.IsValid() && (value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer) {
			if value.IsNil() {
				break
			}
			value = value.Elem()
		}
		if value.IsValid() && value.Kind() != reflect.Pointer && value.Kind() != reflect.Interface {
			indirect = append(indirect, value)
		}
	}
	return indirect
}

func reflectedJSONFieldName(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "" || tag == "-" {
		return ""
	}
	name, _, _ := strings.Cut(tag, ",")
	return name
}

func isCompletenessOmission(typ reflect.Type, field string) bool {
	fields, ok := completenessOmissions[typ]
	if !ok {
		return false
	}
	_, ok = fields[field]
	return ok
}
