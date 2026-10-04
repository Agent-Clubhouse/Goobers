package httpapi

import (
	"errors"
	"net/http"
	"reflect"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

func containedPod(p Principal) bool {
	return IsPodPrincipal(p) && (p.GeneratedChild || p.WorkflowParent)
}

// authorizeContainedPod is an explicit execution-plane ceiling. Ordinary pod
// scopes and operator roles cannot widen a contained execution's authority.
func authorizeContainedPod(r *http.Request, p Principal) error {
	run, ok := strings.CutPrefix(p.Subject, "run:")
	if !ok || !apiv1.ValidRunID(run) || (p.GeneratedChild && p.WorkflowParent) {
		return errors.New("invalid contained pod identity")
	}
	path, method := r.URL.Path, r.Method
	own := apicontract.RunsPath + "/" + run + "/"
	if method == http.MethodPost && (path == apicontract.CredentialResolvePath || path == apicontract.ClaimListPath) {
		return nil
	}
	if blobPlanePath(path) && (method == http.MethodGet || method == http.MethodPut) {
		return nil
	}
	if strings.HasPrefix(path, own) && method == http.MethodPost && (journalPlanePath(path) || surrenderPlanePath(path)) {
		return nil
	}
	if p.WorkflowParent && path == own+"child-workflow-access" && (method == http.MethodPost || method == http.MethodDelete) {
		return nil
	}
	return errors.New("contained pod may only use its bounded execution planes")
}

func validateContainedClaimList(r *http.Request, in ClaimListRequest) error {
	p, ok := PrincipalFromRequest(r)
	if !ok || !containedPod(p) {
		return nil
	}
	if p.Subject != podPrincipalSubject(in.RunID) || in.Scope != ClaimListScopeRun || !in.Execution || in.Gaggle != "" || in.Provider != "" {
		return errors.New("contained pod may only observe its own execution claims")
	}
	return nil
}

func validateContainedJournal(r *http.Request, in livejournal.EmitRequest) error {
	p, ok := PrincipalFromRequest(r)
	if !ok || !containedPod(p) {
		return nil
	}
	if p.Subject != podPrincipalSubject(in.RunID) || in.Open != nil {
		return errors.New("contained pod cannot author journal identity")
	}
	for _, op := range in.Ops {
		if !containedObservation(op) {
			return errors.New("contained pod journal accepts observations only")
		}
	}
	return nil
}

// The daemon wrapper additionally binds every stage/attempt to the retained
// execution contract and supplies a scoped blob store. These checks prevent
// caller-authored lifecycle events or source trust from becoming authority.
func containedObservation(op livejournal.Op) bool {
	switch op.Kind {
	case livejournal.OpAppend:
		return op.Event != nil && op.Artifact == nil && op.Span == nil && op.Checkpoint == nil && containedEvent(*op.Event)
	case livejournal.OpArtifact:
		return op.Artifact != nil && op.Event == nil && op.Span == nil && op.Checkpoint == nil && containedArtifactIntegrity(op.Artifact)
	case livejournal.OpSpan:
		return op.Span != nil && op.Event == nil && op.Artifact == nil && op.Checkpoint == nil
	case livejournal.OpTranscriptCheckpoint:
		return op.Checkpoint != nil && op.Event == nil && op.Artifact == nil && op.Span == nil
	default:
		return false
	}
}

func containedEvent(e journal.Event) bool {
	if e.Stage == "" || e.Branch != 0 {
		return false
	}
	expected := journal.Event{Schema: e.Schema, Seq: e.Seq, Time: e.Time, Type: e.Type, Stage: e.Stage, Attempt: e.Attempt, AttemptClass: e.AttemptClass}
	switch e.Type {
	case journal.EventStageHeartbeat:
	case journal.EventAgentLifecycle, journal.EventAgentMessage, journal.EventAgentProgress:
		expected.Agent, expected.Progress, expected.PeerMessage = e.Agent, e.Progress, e.PeerMessage
	case journal.EventRunnerIsolationPosture:
		expected.Runner = e.Runner
	case journal.EventRunnerAnnotation:
		kind, _ := e.Runner["kind"].(string)
		switch kind {
		case "agent-telemetry-fidelity", "goobers-io-input-inspection-receipts", "credit-span-provenance", "mcp-server-unavailable", "required-mcp-readiness":
			expected.Runner = e.Runner
		default:
			return false
		}
	default:
		return false
	}
	return reflect.DeepEqual(e, expected)
}

func containedArtifactIntegrity(a *livejournal.ArtifactOp) bool {
	integrity := a.Integrity
	if integrity == "" && a.Ref != nil {
		integrity = a.Ref.Integrity
	}
	return integrity == "" || integrity == apiv1.IntegrityDerived || integrity == apiv1.IntegrityUnapproved
}
