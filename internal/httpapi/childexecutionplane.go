package httpapi

import (
	"context"
	"fmt"
)

// ChildExecutionObserver exposes only execution observations. Generated child
// workers never receive claim acquisition, renewal, release or recovery methods.
type ChildExecutionObserver interface {
	List(context.Context, ClaimListRequest) (ClaimListResponse, error)
}

// WithGeneratedChildExecutionObserver installs the exact-attempt owner. The
// ordinary claims service is never a fallback for generated child principals.
func WithGeneratedChildExecutionObserver(observer ChildExecutionObserver) HandlerOption {
	return func(config *handlerConfig) error {
		if observer == nil {
			return fmt.Errorf("httpapi: nil generated child execution observer")
		}
		config.generatedChildExecution = observer
		return nil
	}
}

// WithWorkflowParentExecutionObserver installs the exact active parent owner.
func WithWorkflowParentExecutionObserver(observer ChildExecutionObserver) HandlerOption {
	return func(config *handlerConfig) error {
		if observer == nil {
			return fmt.Errorf("httpapi: nil parent execution observer")
		}
		config.workflowParentExecution = observer
		return nil
	}
}
