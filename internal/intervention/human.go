package intervention

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	gateevaluator "github.com/goobers/goobers/internal/gate"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/journal"
)

const sharedGuidanceMode = "shared-guidance"
const maxSharedGuidanceBytes = 64 << 10
const maxSharedGuidanceRecords = 100

// HumanService binds gate decisions and saved guidance to explicit gaggle
// policy. It delegates execution and durable writes to the existing services.
type HumanService struct {
	interventions *Service
	policy        *interactiveaccess.Service
	messages      httpapi.OperatorMessageService
	scrubber      journal.Scrubber
	mu            sync.Mutex
}

// NewHumanService enables the local-run intervention surface. The scrubber must
// include the daemon's shared credential registry. Engine execution and fresh
// stage restart allowances are intentionally not offered by this service.
func NewHumanService(interventions *Service, policy *interactiveaccess.Service, messages httpapi.OperatorMessageService, scrubber journal.Scrubber) (*HumanService, error) {
	if interventions == nil || policy == nil || messages == nil || scrubber == nil {
		return nil, errors.New("interactive run service requires interventions, policy, shared messages and scrubber")
	}
	return &HumanService{interventions: interventions, policy: policy, messages: messages, scrubber: scrubber}, nil
}

func humanDenied() error {
	return httpapi.NewInterventionError(http.StatusForbidden, "interactive_access_denied", "interactive run access is not authorized", nil)
}
func (s *HumanService) resolve(ctx context.Context, p httpapi.Principal, run string) (resolvedInterventionRun, error) {
	if !apiv1.ValidRunID(run) {
		return resolvedInterventionRun{}, interventionBadRequest("invalid_run_id", "run ID is invalid")
	}
	definitions := s.interventions.definitions()
	gaggles := make([]string, 0, len(definitions.Runners))
	for gaggle := range definitions.Runners {
		gaggles = append(gaggles, gaggle)
	}
	dir, gaggle, err := s.interventions.locateRun(gaggles, run, false)
	if err != nil {
		return resolvedInterventionRun{}, err
	}
	permissions, err := s.policy.InteractiveCapabilities(ctx, p, gaggle)
	if err != nil || !permissions.Viewer {
		return resolvedInterventionRun{}, humanDenied()
	}
	// The first surface reuses the retained event scan. Refuse oversized journals
	// instead of allowing an interactive request to allocate unbounded history.
	info, err := os.Stat(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return resolvedInterventionRun{}, err
	}
	if info.Size() > 32<<20 {
		return resolvedInterventionRun{}, interventionConflict("interactive_history_too_large", "This run exceeds the interactive history read budget.")
	}
	resolved, err := s.interventions.resolve(run)
	if err != nil {
		return resolvedInterventionRun{}, err
	}
	if resolved.gaggle != gaggle {
		return resolvedInterventionRun{}, humanDenied()
	}
	return resolved, nil
}

// InspectInteractiveRun returns current gate occurrences and the last 100 saved
// guidance records. Existing read-only monitoring has its separate service.
func (s *HumanService) InspectInteractiveRun(ctx context.Context, p httpapi.Principal, run string) (apicontract.InteractiveRunView, error) {
	resolved, err := s.resolve(ctx, p, run)
	if err != nil {
		return apicontract.InteractiveRunView{}, err
	}
	result := apicontract.InteractiveRunView{RunID: run, Gaggle: resolved.gaggle, Phase: string(resolved.phase), Actions: []apicontract.InteractiveRunAction{}, Guidance: []apiv1.OperatorMessageRecord{}, RestartReason: "Stage restart with a fresh allowance is not available on this daemon yet."}
	for _, record := range journal.ReplayOperatorMessages(resolved.events) {
		if record.Request.DeliveryMode == sharedGuidanceMode {
			result.Guidance = append(result.Guidance, record)
		}
	}
	if len(result.Guidance) > maxSharedGuidanceRecords {
		result.Guidance = result.Guidance[len(result.Guidance)-maxSharedGuidanceRecords:]
	}
	if resolved.engineDriven {
		result.RestartReason = "This interactive surface currently supports local runs. Engine run interventions are not enabled here."
		return result, nil
	}
	allowed := s.policy.Authorize(p, resolved.gaggle, "run.intervene") == nil
	result.Actions = humanActions(resolved, allowed, p.Subject)
	return result, nil
}

func humanActions(resolved resolvedInterventionRun, allowed bool, actor string) []apicontract.InteractiveRunAction {
	actions := []apicontract.InteractiveRunAction{}
	reason := ""
	if !allowed {
		reason = "Your gaggle policy does not authorize run intervention."
	}
	add := func(kind, stage string, seq uint64, decisions []string) {
		actions = append(actions, apicontract.InteractiveRunAction{Kind: kind, Stage: stage, SubjectSequence: seq, Decisions: decisions, Available: allowed, Reason: reason})
	}
	for _, gate := range resolved.machine.Def.Spec.Gates {
		seq := humanSubject(resolved, gate.Name)
		if seq == 0 {
			continue
		}
		add("guidance", gate.Name, seq, []string{})
		if gate.Evaluator != apiv1.EvaluatorHuman && gate.Evaluator != apiv1.EvaluatorAgentic {
			continue
		}
		decisions := humanDecisions(resolved, gate, actor)
		if len(decisions) > 0 {
			add("approve", gate.Name, seq, decisions)
		}
		if resolved.phase == journal.PhaseEscalated || resolved.phase == journal.PhaseFailed {
			if len(decisions) > 0 {
				add("override", gate.Name, seq, decisions)
			}
			add("deny", gate.Name, seq, []string{})
		}
	}
	for _, task := range resolved.machine.Def.Spec.Tasks {
		if seq := humanSubject(resolved, task.Name); seq > 0 {
			add("guidance", task.Name, seq, []string{})
			if task.Name == terminalHumanSubject(resolved.events) {
				add("deny", task.Name, seq, []string{})
			}
		}
	}
	return actions
}

func humanDecisions(resolved resolvedInterventionRun, gate apiv1.Gate, actor string) []string {
	result := []string{}
	for decision := range gate.Branches {
		if gate.Evaluator == apiv1.EvaluatorHuman && gateevaluator.ValidateHumanDecision(gate, decision, actor) != nil {
			continue
		}
		if _, _, err := interventionBranch(resolved.machine, resolved.events, gate.Name, decision); err == nil {
			result = append(result, decision)
		}
	}
	slices.Sort(result)
	return result
}

// humanSubject binds terminal commands to the terminal generation and paused
// commands to the precise unresolved gate occurrence. Active execution is not
// a supported target for saved guidance on this surface.
func humanSubject(resolved resolvedInterventionRun, stage string) uint64 {
	if resolved.phase == journal.PhaseRunning {
		seq, ok := unresolvedGatePause(resolved.events, stage)
		if ok {
			return seq
		}
		return 0
	}
	if resolved.phase != journal.PhaseEscalated && resolved.phase != journal.PhaseFailed {
		return 0
	}
	var terminal uint64
	seen := false
	for i := len(resolved.events) - 1; i >= 0; i-- {
		event := resolved.events[i]
		if event.Type == journal.EventRunResumed || event.Type == journal.EventStageRerunRequested {
			break
		}
		if terminal == 0 && event.Type == journal.EventRunFinished {
			terminal = event.Seq
		}
		if event.Stage == stage || event.Gate == stage {
			seen = true
		}
	}
	if seen {
		return terminal
	}
	return 0
}

func checkHumanSubject(resolved resolvedInterventionRun, input httpapi.InterventionRequest) error {
	if input.ExpectedSubjectSequence == 0 {
		return nil
	}
	if humanSubject(resolved, input.Stage) != input.ExpectedSubjectSequence {
		return interventionConflict("interactive_subject_changed", "The stage occurrence changed. Refresh before issuing a new command.")
	}
	return nil
}

// AcceptInteractiveRun resolves identity and scope without trusting request
// actor/gaggle fields. A pending result is not proof of an applied decision.
func (s *HumanService) AcceptInteractiveRun(admission, execution context.Context, p httpapi.Principal, run, key string, input apicontract.InteractiveRunCommand) (apicontract.InteractiveRunCommandResult, error) {
	if err := validateHumanCommand(key, input); err != nil {
		return apicontract.InteractiveRunCommandResult{}, err
	}
	resolved, err := s.resolve(admission, p, run)
	if err != nil {
		return apicontract.InteractiveRunCommandResult{}, err
	}
	if resolved.engineDriven {
		return apicontract.InteractiveRunCommandResult{}, interventionConflict("interactive_engine_unsupported", "this interactive surface currently supports local runs")
	}
	// Scrub before fingerprints and persistence; no raw human content is logged.
	input.Guidance = string(s.scrubber.Scrub([]byte(input.Guidance)))
	input.Rationale = string(s.scrubber.Scrub([]byte(input.Rationale)))
	var result apicontract.InteractiveRunCommandResult
	err = s.policy.WithAuthorization(admission, p, resolved.gaggle, "run.intervene", func(ctx context.Context) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		var callErr error
		if input.Kind == "guidance" {
			result, callErr = s.saveGuidance(ctx, p, resolved, key, input)
		} else {
			result, callErr = s.decide(ctx, execution, p, resolved, key, input)
		}
		return callErr
	})
	if errors.Is(err, interactiveaccess.ErrDenied) {
		err = humanDenied()
	}
	return result, err
}

func validateHumanCommand(key string, input apicontract.InteractiveRunCommand) error {
	if strings.TrimSpace(key) == "" || len(key) > 200 || input.ExpectedSubjectSequence == 0 || strings.TrimSpace(input.Stage) == "" || len(input.Stage) > 256 {
		return interventionBadRequest("invalid_interactive_command", "Idempotency key, stage and observed subject sequence are required.")
	}
	if len(input.Guidance) > maxSharedGuidanceBytes || len(input.Rationale) > 4096 {
		return interventionBadRequest("interactive_content_too_large", "Guidance must be at most 64 KiB and rationale at most 4096 bytes.")
	}
	switch input.Kind {
	case "guidance":
		if strings.TrimSpace(input.Guidance) == "" || input.Decision != "" || input.Rationale != "" {
			return interventionBadRequest("invalid_interactive_command", "Saved guidance requires text only.")
		}
	case "approve", "override":
		if input.Guidance != "" || input.Decision == "" {
			return interventionBadRequest("invalid_interactive_command", "A gate decision is required; save guidance separately.")
		}
	case "deny":
		if input.Guidance != "" || input.Decision != "" {
			return interventionBadRequest("invalid_interactive_command", "Escalation denial requires rationale only.")
		}
	default:
		return interventionBadRequest("invalid_interactive_command", "Unknown interactive command.")
	}
	if (input.Kind == "override" || input.Kind == "deny") && strings.TrimSpace(input.Rationale) == "" {
		return interventionBadRequest("rationale_required", "A rationale is required.")
	}
	return nil
}

func principalIdentity(p httpapi.Principal) string { return p.Issuer + ":" + p.Subject }
func scopedHumanKey(p httpapi.Principal, gaggle, key string) string {
	return fmt.Sprintf("human:%x", sha256.Sum256([]byte(p.Issuer+"\x00"+p.Subject+"\x00"+gaggle+"\x00"+key)))
}

func (s *HumanService) saveGuidance(ctx context.Context, p httpapi.Principal, resolved resolvedInterventionRun, key string, input apicontract.InteractiveRunCommand) (apicontract.InteractiveRunCommandResult, error) {
	current, err := s.interventions.resolve(resolved.runID)
	if err != nil {
		return apicontract.InteractiveRunCommandResult{}, err
	}
	key = scopedHumanKey(p, current.gaggle, key)
	target := fmt.Sprintf("stage:%s@%d", input.Stage, input.ExpectedSubjectSequence)
	records := journal.ReplayOperatorMessages(current.events)
	for _, record := range records {
		if record.Request.IdempotencyKey == key {
			if record.Request.TargetAddress != target || record.Request.Content.Text != input.Guidance || record.Request.PrincipalRef != principalIdentity(p) || record.Request.DeliveryMode != sharedGuidanceMode {
				return apicontract.InteractiveRunCommandResult{}, interventionConflict("idempotency_key_reused", "Idempotency-Key was already used for different guidance.")
			}
			return guidanceResult(current, record), nil
		}
	}
	for _, event := range current.events {
		if interventionMarkerKey(event) == key || event.Runner["idempotencyKey"] == key {
			return apicontract.InteractiveRunCommandResult{}, interventionConflict("idempotency_key_reused", "Idempotency-Key was already used for another command.")
		}
	}
	if err := checkHumanSubject(current, httpapi.InterventionRequest{Stage: input.Stage, ExpectedSubjectSequence: input.ExpectedSubjectSequence}); err != nil {
		return apicontract.InteractiveRunCommandResult{}, err
	}
	sharedCount := 0
	for _, record := range records {
		if record.Request.DeliveryMode == sharedGuidanceMode {
			sharedCount++
		}
	}
	if sharedCount >= maxSharedGuidanceRecords {
		return apicontract.InteractiveRunCommandResult{}, interventionConflict("guidance_limit_reached", "The run's shared guidance limit has been reached.")
	}
	response, err := s.messages.SubmitOperatorMessage(ctx, httpapi.OperatorMessageSubmissionRequest{Principal: p, OperatorMessageSubmitRequest: apicontract.OperatorMessageSubmitRequest{RunID: current.runID, IdempotencyKey: key, PrincipalRef: principalIdentity(p), Gaggle: current.gaggle, TargetAddress: target, Purpose: "stage-restart-guidance", DeliveryMode: sharedGuidanceMode, Content: apiv1.OperatorMessageContent{Text: input.Guidance}}})
	if err != nil {
		return apicontract.InteractiveRunCommandResult{}, err
	}
	return guidanceResult(current, response.Record), nil
}

func guidanceResult(resolved resolvedInterventionRun, record apiv1.OperatorMessageRecord) apicontract.InteractiveRunCommandResult {
	return apicontract.InteractiveRunCommandResult{Status: "saved", Accepted: true, RunID: resolved.runID, Phase: string(resolved.phase), Guidance: &record}
}

func (s *HumanService) decide(ctx, execution context.Context, p httpapi.Principal, resolved resolvedInterventionRun, key string, input apicontract.InteractiveRunCommand) (apicontract.InteractiveRunCommandResult, error) {
	current, resolveErr := s.interventions.resolve(resolved.runID)
	if resolveErr != nil {
		return apicontract.InteractiveRunCommandResult{}, resolveErr
	}
	resolved = current
	request := httpapi.InterventionRequest{RunID: resolved.runID, Stage: input.Stage, IdempotencyKey: scopedHumanKey(p, resolved.gaggle, key), Actor: p.Subject, PrincipalRef: principalIdentity(p), ExpectedSubjectSequence: input.ExpectedSubjectSequence, Decision: input.Decision, Rationale: input.Rationale}
	for _, record := range journal.ReplayOperatorMessages(resolved.events) {
		if record.Request.IdempotencyKey == request.IdempotencyKey {
			return apicontract.InteractiveRunCommandResult{}, interventionConflict("idempotency_key_reused", "Idempotency-Key was already used for guidance.")
		}
	}
	recorded, failed, replayErr := humanRequestState(resolved.events, input.Kind, request)
	if replayErr != nil {
		return apicontract.InteractiveRunCommandResult{}, replayErr
	}
	if failed {
		return humanFailedResult(resolved), nil
	}
	var err error
	switch input.Kind {
	case "approve":
		_, err = s.interventions.AcceptApprove(ctx, execution, request)
	case "override":
		_, err = s.interventions.AcceptOverride(ctx, execution, request)
	case "deny":
		_, err = s.interventions.AcceptDenyEscalation(ctx, execution, request)
	}
	if err != nil {
		var refusal *httpapi.InterventionError
		if !recorded || !errors.As(err, &refusal) || refusal.Code != "intervention_in_progress" {
			return apicontract.InteractiveRunCommandResult{}, err
		}
	}
	return s.observeDecision(ctx, resolved, input.Kind, request)
}

func (s *HumanService) observeDecision(ctx context.Context, resolved resolvedInterventionRun, kind string, request httpapi.InterventionRequest) (apicontract.InteractiveRunCommandResult, error) {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	result := apicontract.InteractiveRunCommandResult{Status: "pending", RunID: resolved.runID, Phase: string(resolved.phase)}
	for {
		reader, err := journal.OpenRead(resolved.runDir)
		if err != nil {
			return result, err
		}
		resolved.events, err = reader.Events()
		if err != nil {
			return result, err
		}
		_, failed, stateErr := humanRequestState(resolved.events, kind, request)
		if stateErr != nil {
			return result, stateErr
		}
		if failed {
			return humanFailedResult(resolved), nil
		}
		applied := false
		if kind == "deny" {
			fingerprint, _ := interventionFingerprint(kind, request)
			applied, err = scanEscalationResolution(resolved.events, request.IdempotencyKey, fingerprint)
		} else {
			_, applied, err = replayIntervention(resolved, kind, request)
		}
		if err != nil {
			return result, err
		}
		if applied {
			position, err := currentInterventionResult(resolved)
			if err != nil {
				return result, err
			}
			result.Status = "applied"
			result.Accepted = true
			result.Phase = position.Phase
			result.JournalSequence = position.JournalSeq
			return result, nil
		}
		select {
		case <-ctx.Done():
			return result, nil
		case <-ticker.C:
		}
	}
}

// humanRequestState shares a key namespace across decisions, denial and failure
// receipts. A later failure is durable evidence, never inferred from a timeout.
func humanRequestState(events []journal.Event, action string, input httpapi.InterventionRequest) (recorded, failed bool, err error) {
	fingerprint, err := interventionFingerprint(action, input)
	if err != nil {
		return false, false, err
	}
	for _, event := range events {
		if event.Runner["idempotencyKey"] != input.IdempotencyKey {
			continue
		}
		kind := event.Runner["kind"]
		if kind != interventionIdempotencyMarker && kind != escalationResolutionMarker && kind != "intervention.failed" {
			continue
		}
		if event.Runner["fingerprint"] != fingerprint {
			return false, false, interventionConflict("idempotency_key_reused", "Idempotency-Key was already used for a different command.")
		}
		recorded = true
		if kind == "intervention.failed" {
			failed = true
		}
	}
	return recorded, failed, nil
}

func humanFailedResult(resolved resolvedInterventionRun) apicontract.InteractiveRunCommandResult {
	result := apicontract.InteractiveRunCommandResult{Status: "failed", RunID: resolved.runID, Phase: string(resolved.phase)}
	if len(resolved.events) > 0 {
		result.JournalSequence = resolved.events[len(resolved.events)-1].Seq
	}
	return result
}

func recordHumanFailure(resolved resolvedInterventionRun, action string, input httpapi.InterventionRequest) error {
	if input.PrincipalRef == "" {
		return nil
	}
	fingerprint, err := interventionFingerprint(action, input)
	if err != nil {
		return err
	}
	reader, err := journal.OpenRead(resolved.runDir)
	if err != nil {
		return err
	}
	events, err := reader.Events()
	if err != nil {
		return err
	}
	// Failure after the decision applied belongs to the resumed execution, not
	// to this command's receipt. Its own stage events retain that failure.
	markerSeen := false
	for _, event := range events {
		if interventionMarkerKey(event) == input.IdempotencyKey {
			markerSeen = true
		}
		if markerSeen && interventionCompleted(action, input.Stage, event) {
			return nil
		}
	}
	_, scrubber := journal.DefaultScrubber()
	run, _, err := journal.Recover(resolved.runDir, journal.WithScrubber(scrubber))
	if err != nil {
		return err
	}
	defer func() { _ = run.Close() }()
	return run.Append(journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "intervention.failed", "idempotencyKey": input.IdempotencyKey, "fingerprint": fingerprint, "stage": input.Stage, "principalRef": input.PrincipalRef, "action": action}})
}

func terminalHumanSubject(events []journal.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type == journal.EventGateEvaluated {
			return event.Gate
		}
		if event.Type == journal.EventStageFinished {
			return event.Stage
		}
		if event.Type == journal.EventRunResumed {
			break
		}
	}
	return ""
}
