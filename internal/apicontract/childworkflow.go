package apicontract

import "github.com/goobers/goobers/internal/apicontract/childworkflowwire"

// ChildWorkflowSourceRequest carries authored DSL only.
type ChildWorkflowSourceRequest = childworkflowwire.ChildWorkflowSourceRequest

// ChildWorkflowStatusRequest selects a caller-owned invocation.
type ChildWorkflowStatusRequest = childworkflowwire.ChildWorkflowStatusRequest

// ChildWorkflowDiagnostic is a bounded authoring refusal.
type ChildWorkflowDiagnostic = childworkflowwire.ChildWorkflowDiagnostic

// ChildWorkflowValidationResponse reports an advisory check.
type ChildWorkflowValidationResponse = childworkflowwire.ChildWorkflowValidationResponse

// ChildWorkflowResponse reports durable custody independently of execution.
type ChildWorkflowResponse = childworkflowwire.ChildWorkflowResponse

// ChildWorkflowResolveRequest chooses one exact child result disposition.
type ChildWorkflowResolveRequest = childworkflowwire.ChildWorkflowResolveRequest

// ChildWorkflowResolutionResponse separates accepted intent from application.
type ChildWorkflowResolutionResponse = childworkflowwire.ChildWorkflowResolutionResponse

// MaxChildWorkflowSourceBytes caps source bytes in every transport.
const MaxChildWorkflowSourceBytes = childworkflowwire.MaxChildWorkflowSourceBytes

// MaxChildWorkflowInvocationKeyBytes caps the occurrence-scoped retry key.
const MaxChildWorkflowInvocationKeyBytes = childworkflowwire.MaxChildWorkflowInvocationKeyBytes
