package intervention

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
)

type humanTestMessages struct{ dir string }

func (s humanTestMessages) SubmitOperatorMessage(_ context.Context, in httpapi.OperatorMessageSubmissionRequest) (httpapi.OperatorMessageSubmissionResponse, error) {
	run, _, err := journal.Recover(s.dir)
	if err != nil {
		return httpapi.OperatorMessageSubmissionResponse{}, err
	}
	defer func() { _ = run.Close() }()
	record, _, err := run.AcceptOperatorMessage(apiv1.OperatorMessageRequest{Schema: apiv1.OperatorMessageRequestSchema, RequestID: in.IdempotencyKey, IdempotencyKey: in.IdempotencyKey, RequestedAt: time.Now(), TargetAddress: in.TargetAddress, PrincipalRef: in.PrincipalRef, Purpose: in.Purpose, DeliveryMode: in.DeliveryMode, Content: in.Content})
	return httpapi.OperatorMessageSubmissionResponse{Accepted: err == nil, Record: record}, err
}
func humanFixture(t *testing.T, terminal bool) (*HumanService, httpapi.Principal, string) {
	t.Helper()
	events := []journal.Event{{Type: journal.EventStageStarted, Stage: "implement", Attempt: 1}, {Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Status: string(apiv1.ResultSuccess)}, {Type: journal.EventGateStarted, Gate: "review"}}
	if terminal {
		events = append(events, journal.Event{Type: journal.EventGateEvaluated, Gate: "review", Verdict: "fail", Target: "escalate"}, journal.Event{Type: journal.EventRunFinished, Status: string(journal.PhaseEscalated)})
	} else {
		events = append(events, journal.Event{Type: journal.EventGatePaused, Gate: "review"})
	}
	legacy, dir := newInterventionServiceTestRun(t, interventionTestMachine(t, apiv1.EvaluatorHuman), "human-run", events)
	var wg sync.WaitGroup
	legacy.wg = &wg
	t.Cleanup(wg.Wait)
	reg, scrubber := journal.DefaultScrubber()
	reg.Register([]byte("human-secret-canary"))
	gaggle := apiv1.Gaggle{ObjectMeta: metav1.ObjectMeta{Name: "example"}, Spec: apiv1.GaggleSpec{InteractiveAccess: &apiv1.InteractiveAccessPolicy{Humans: apiv1.InteractiveHumanGrants{Viewers: []apiv1.InteractiveHumanGrant{{Issuer: "https://identity.example", Subject: "viewer"}}, Operators: []apiv1.InteractiveHumanGrant{{Issuer: "https://identity.example", Subject: "alice"}}}, Actions: []apiv1.InteractiveAction{"run.intervene"}}}}
	policy, err := interactiveaccess.New([]apiv1.Gaggle{gaggle}, nil, interactiveaccess.Dependencies{Registrar: reg})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewHumanService(legacy, policy, humanTestMessages{dir}, scrubber)
	if err != nil {
		t.Fatal(err)
	}
	return service, httpapi.Principal{Issuer: "https://identity.example", Subject: "alice", Roles: []httpapi.Role{httpapi.RoleOperate}}, dir
}
func humanCommand(t *testing.T, s *HumanService, p httpapi.Principal, kind string) apicontract.InteractiveRunCommand {
	t.Helper()
	view, err := s.InspectInteractiveRun(context.Background(), p, "human-run")
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range view.Actions {
		if action.Kind == kind && action.Stage == "review" {
			return apicontract.InteractiveRunCommand{Kind: kind, Stage: action.Stage, ExpectedSubjectSequence: action.SubjectSequence, Decision: "pass"}
		}
	}
	t.Fatalf("no %s action: %+v", kind, view)
	return apicontract.InteractiveRunCommand{}
}
func TestHumanApprovalUsesRealRunnerAndOccurrence(t *testing.T) {
	s, p, dir := humanFixture(t, false)
	input := humanCommand(t, s, p, "approve")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stale := input
	stale.ExpectedSubjectSequence++
	if _, err := s.AcceptInteractiveRun(ctx, ctx, p, "human-run", "stale", stale); err == nil {
		t.Fatal("stale approval accepted")
	}
	result, err := s.AcceptInteractiveRun(ctx, ctx, p, "human-run", "approve", input)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.Status != "applied" || result.JournalSequence == 0 {
		t.Fatalf("result=%+v", result)
	}
	replay, err := s.AcceptInteractiveRun(ctx, ctx, p, "human-run", "approve", input)
	if err != nil || replay.Status != "applied" {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	reader, _ := journal.OpenRead(dir)
	events, _ := reader.Events()
	markers := 0
	for _, event := range events {
		if interventionMarkerKey(event) != "" {
			markers++
			if event.Runner["principalRef"] != "https://identity.example:alice" {
				t.Fatalf("missing principal: %+v", event)
			}
		}
	}
	if markers != 1 {
		t.Fatalf("markers=%d", markers)
	}
	input.Decision = "fail"
	if _, err := s.AcceptInteractiveRun(ctx, ctx, p, "human-run", "approve", input); err == nil {
		t.Fatal("changed payload replayed")
	}
}
func TestHumanGuidanceIsSavedScrubbedSharedAndNotDelivered(t *testing.T) {
	s, p, dir := humanFixture(t, true)
	input := humanCommand(t, s, p, "guidance")
	input.Decision = ""
	input.Guidance = "Inspect logs human-secret-canary before retrying."
	result, err := s.AcceptInteractiveRun(context.Background(), context.Background(), p, "human-run", "note", input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "saved" || result.Guidance.Outcome != nil || result.Guidance.Request.DeliveryMode != sharedGuidanceMode {
		t.Fatalf("result=%+v", result)
	}
	viewer := p
	viewer.Subject = "viewer"
	viewer.Roles = []httpapi.Role{httpapi.RoleView}
	view, err := s.InspectInteractiveRun(context.Background(), viewer, "human-run")
	if err != nil || len(view.Guidance) != 1 {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	if view.Guidance[0].Request.PrincipalRef != principalIdentity(p) || strings.Contains(view.Guidance[0].Request.Content.Text, "human-secret-canary") {
		t.Fatal("attribution/redaction lost")
	}
	for _, action := range view.Actions {
		if action.Available {
			t.Fatal("viewer offered a write")
		}
	}
	if _, err := s.AcceptInteractiveRun(context.Background(), context.Background(), p, "human-run", "note", input); err != nil {
		t.Fatal(err)
	}
	input.Guidance = "changed"
	if _, err := s.AcceptInteractiveRun(context.Background(), context.Background(), p, "human-run", "note", input); err == nil {
		t.Fatal("changed note reused key")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil || strings.Contains(string(raw), "human-secret-canary") {
		t.Fatal("raw secret persisted")
	}
	reader, _ := journal.OpenRead(dir)
	phase, _ := reader.Phase()
	if phase != journal.PhaseEscalated {
		t.Fatalf("guidance changed phase to %s", phase)
	}
}
func TestHumanPolicyNeverFallsBackToInstanceAdmin(t *testing.T) {
	s, p, _ := humanFixture(t, true)
	input := humanCommand(t, s, p, "deny")
	input.Decision = ""
	input.Rationale = "Reviewed and declined."
	for _, subject := range []string{"outsider", "viewer"} {
		p.Subject = subject
		p.Roles = []httpapi.Role{httpapi.RoleAdmin}
		_, err := s.AcceptInteractiveRun(context.Background(), context.Background(), p, "human-run", "deny", input)
		var refusal *httpapi.InterventionError
		if !errors.As(err, &refusal) || refusal.Status != 403 {
			t.Fatalf("subject=%s err=%v", subject, err)
		}
	}
	p.Subject = "alice"
	result, err := s.AcceptInteractiveRun(context.Background(), context.Background(), p, "human-run", "deny", input)
	if err != nil || result.Status != "applied" || result.Phase != "escalated" {
		t.Fatalf("deny=%+v err=%v", result, err)
	}
}
