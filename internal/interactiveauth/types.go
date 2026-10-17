// Package interactiveauth implements the experimental, mediated delegated-access
// boundary. Its only providers are explicitly controlled local demonstrations.
// It never acquires Microsoft credentials and never falls back to application identity.
package interactiveauth

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Provider string
type Action string

const (
	ADO             Provider = "ado"
	AppInsights     Provider = "app_insights"
	ReadWorkItem    Action   = "ado.work_item.read"
	AddComment      Action   = "ado.work_item.comment"
	QueryTelemetry  Action   = "azure.app_insights.query"
	ControlledLocal          = "controlled-local"
)

// Identity contains audit claims, not credentials. Display names never identify owners.
type Identity struct {
	Issuer   string `json:"issuer"`
	Subject  string `json:"subject"`
	TenantID string `json:"tenantId"`
	ObjectID string `json:"objectId"`
	Cloud    string `json:"cloud"`
}

// Principal is deliberately not wire-deserializable. Only a trusted server adapter
// may choose its local owner; an HTTP request body cannot manufacture this value.
type Principal struct {
	identity Identity
	verified bool
}

// LocalPrincipal binds the explicit controlled-local server identity. It does not
// assert an Entra authentication. Never pass an owner supplied in an HTTP body/header.
func LocalPrincipal(serverOwner string) (Principal, error) {
	if strings.TrimSpace(serverOwner) == "" || len(serverOwner) > 256 {
		return Principal{}, fault("unauthenticated", "a server-authenticated local owner is required")
	}
	return Principal{Identity{Issuer: "goobers:controlled-local", Subject: serverOwner, TenantID: "demo-tenant", ObjectID: serverOwner, Cloud: ControlledLocal}, true}, nil
}
func (p Principal) Identity() Identity { return p.identity }

// Resource is an exact canonical tuple, never a wildcard or prefix permission.
type Resource struct {
	Provider      Provider `json:"provider"`
	Organization  string   `json:"organization,omitempty"`
	Project       string   `json:"project,omitempty"`
	Repository    string   `json:"repository,omitempty"`
	Subscription  string   `json:"subscription,omitempty"`
	ResourceGroup string   `json:"resourceGroup,omitempty"`
	AppInsights   string   `json:"appInsights,omitempty"`
}

func DemoADOResource() Resource {
	return Resource{Provider: ADO, Organization: "goobers-demo", Project: "interactive", Repository: "sample-app"}
}
func DemoInsightsResource() Resource {
	return Resource{Provider: AppInsights, Subscription: "demo-subscription", ResourceGroup: "interactive", AppInsights: "sample-app"}
}

type ConnectionRef struct {
	ID       string `json:"connectionId"`
	Revision uint64 `json:"bindingRevision"`
}
type AuthorityRef struct {
	ID      string `json:"authorityId"`
	Version uint64 `json:"authorityVersion"`
}
type Binding struct {
	Connection ConnectionRef `json:"connectionRef"`
	Resources  []Resource    `json:"resources"`
}

type Connection struct {
	Ref          ConnectionRef `json:"ref"`
	Owner        Identity      `json:"owner"`
	Provider     Provider      `json:"provider"`
	Resources    []Resource    `json:"resources"`
	Mode         string        `json:"mode"`
	Label        string        `json:"label"`
	State        string        `json:"state"` // ready, expired, interaction_required, consent_required, revoked
	ExpiresAt    time.Time     `json:"expiresAt"`
	RefreshCount int           `json:"refreshCount"`
	Epoch        uint64        `json:"epoch"`
}

type Authority struct {
	Ref            AuthorityRef  `json:"ref"`
	Actor          Identity      `json:"actor"`
	SessionID      string        `json:"sessionId"`
	ExecutionID    string        `json:"executionId"`
	ExecutionFence uint64        `json:"executionFence"`
	Parent         *AuthorityRef `json:"parentAuthorityRef,omitempty"`
	Bindings       []Binding     `json:"bindings"`
	AllowedActions []Action      `json:"allowedActions"`
	IssuedAt       time.Time     `json:"issuedAt"`
	NotAfter       time.Time     `json:"notAfter"`
	SharedBudgetID string        `json:"sharedBudgetId"`
	MaxChildren    int           `json:"maxChildren"`
	MaxDepth       int           `json:"maxDepth"`
	Depth          int           `json:"depth"`
	Mode           string        `json:"identityMode"`
}

type AuthorityRequest struct {
	SessionID      string
	ExecutionID    string
	ExecutionFence uint64
	Bindings       []Binding
	AllowedActions []Action
	NotAfter       time.Time
	Budget         int
	MaxChildren    int
	MaxDepth       int
}

type Budget struct {
	Limit int `json:"limit"`
	Used  int `json:"used"`
}
type AuthorityStatus struct {
	Authority Authority `json:"authority"`
	State     string    `json:"state"`
	Budget    Budget    `json:"budget"`
	Epoch     uint64    `json:"epoch"`
}

// Payload uses native provider concepts instead of accepting arbitrary URLs/HTTP.
type Payload struct {
	WorkItemID int    `json:"workItemId,omitempty"`
	Comment    string `json:"comment,omitempty"`
	Query      string `json:"query,omitempty"`
}

type OperationRequest struct {
	Authority      AuthorityRef `json:"authorityRef"`
	SessionID      string       `json:"sessionId"`
	ExecutionID    string       `json:"executionId"`
	ExecutionFence uint64       `json:"executionFence"`
	TurnID         string       `json:"turnId,omitempty"`
	LeaseID        string       `json:"leaseId,omitempty"`
	OperationID    string       `json:"operationId"`
	ActionID       string       `json:"actionId"`
	IdempotencyKey string       `json:"idempotencyKey"`
	Action         Action       `json:"action"`
	Resource       Resource     `json:"resource"`
	Payload        Payload      `json:"payload"`
	DecisionID     string       `json:"decisionId,omitempty"`
	// SimulateLostResponse is an explicit controlled-provider fault injection.
	// It never targets a live provider and is included in exact approval intent.
	SimulateLostResponse bool `json:"simulateLostResponse,omitempty"`
}

type Decision struct {
	ID            string       `json:"id"`
	Authority     AuthorityRef `json:"authorityRef"`
	OperationID   string       `json:"operationId"`
	PayloadDigest string       `json:"payloadDigest"`
	Approver      Identity     `json:"approver"`
	NotAfter      time.Time    `json:"notAfter"`
	CreatedAt     time.Time    `json:"createdAt"`
}

type Operation struct {
	ID                string         `json:"id"`
	Authority         AuthorityRef   `json:"authorityRef"`
	SessionID         string         `json:"sessionId"`
	ExecutionID       string         `json:"executionId"`
	ExecutionFence    uint64         `json:"executionFence"`
	ActionID          string         `json:"actionId"`
	Action            Action         `json:"action"`
	Resource          Resource       `json:"resource"`
	PayloadDigest     string         `json:"payloadDigest"`
	IdempotencyKey    string         `json:"idempotencyKey"`
	State             string         `json:"state"` // dispatched, confirmed, failed, outcome_unknown
	Mode              string         `json:"mode"`
	Connection        ConnectionRef  `json:"connectionRef"`
	AuthorityEpoch    uint64         `json:"authorityEpoch"`
	ConnectionEpoch   uint64         `json:"connectionEpoch"`
	DispatchedAt      time.Time      `json:"dispatchedAt"`
	FinishedAt        *time.Time     `json:"finishedAt,omitempty"`
	ProviderRequestID string         `json:"providerRequestId,omitempty"`
	Result            map[string]any `json:"result,omitempty"`
	ErrorCode         string         `json:"errorCode,omitempty"`
	Message           string         `json:"message,omitempty"`
}

type Config struct {
	// Path must be outside the repository. Empty means in-memory tests only.
	Path string
	Now  func() time.Time
	// CheckExecution checks the current server writer lease and fence immediately
	// before dispatch. Identity comes from verified authority, never the request.
	// Called under the broker mutex; session code must not call the broker while
	// holding its own lifecycle mutex. Nil is for isolated broker qualification.
	CheckExecution func(context.Context, Identity, OperationRequest) error
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string         { return e.Code + ": " + e.Message }
func fault(code, message string) error { return &Error{Code: code, Message: message} }
func ErrorCode(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	if err != nil {
		return "unavailable"
	}
	return ""
}

func contextOK(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fault("cancelled", "operation cancelled before admission")
	}
	return nil
}
func requiredID(value, field string) error {
	if strings.TrimSpace(value) == "" || len(value) > 256 {
		return fault("invalid", fmt.Sprintf("%s is required and must be at most 256 characters", field))
	}
	return nil
}
