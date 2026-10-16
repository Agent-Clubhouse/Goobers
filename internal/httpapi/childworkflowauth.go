package httpapi

import (
	"errors"
	"net/http"
	"strings"
)

// ChildWorkflowPrincipalIssuer is the stage-only child operation trust domain.
const ChildWorkflowPrincipalIssuer = "goobers/child-workflow"

// ChildWorkflowPrincipal is authenticated transport context, never request-body
// data. Services must additionally verify that the grant is still bound to the
// current attempt, pinned policy and gaggle before reading or changing custody.
type ChildWorkflowPrincipal struct {
	GrantID         string
	Gaggle          string
	RunID           string
	StageOccurrence string
	AttemptID       string
	ConfigDigest    string
	PolicyDigest    string
}

func authorizeChildWorkflow(request *http.Request, principal Principal) error {
	claim := principal.ChildWorkflow
	if claim == nil || claim.GrantID == "" || claim.Gaggle == "" || claim.RunID == "" ||
		claim.StageOccurrence == "" || claim.AttemptID == "" || claim.ConfigDigest == "" || claim.PolicyDigest == "" ||
		principal.Subject != "run:"+claim.RunID {
		return errors.New("child-workflow principal has no occurrence binding")
	}
	// The adapter selects a finite operation; arbitrary prefixes, child IDs,
	// sibling run paths and method overrides never widen a stage grant.
	prefix := RunsPath + "/" + claim.RunID + "/child-workflows/"
	operation, ok := strings.CutPrefix(request.URL.Path, prefix)
	if !ok || request.Method != http.MethodPost {
		return errors.New("child-workflow principal may only call its own child operations")
	}
	switch operation {
	case "validate", "start", "status", "await", "resolve":
		return nil
	default:
		return errors.New("unsupported child-workflow operation")
	}
}
