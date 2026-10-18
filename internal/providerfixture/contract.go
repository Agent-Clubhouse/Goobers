package providerfixture

import (
	"context"
	"fmt"
)

type mappedContractProvider[T any] struct {
	list func(context.Context) ([]T, error)
	get  func(context.Context) (T, error)
}

type contractBackend[T any] interface {
	provider(HTTPClient) mappedContractProvider[T]
	targetID() string
	listOperation() string
	getOperation() string
	itemID(T) string
	assertIdentity(T) error
	assertRequiredFields(T) error
	assertConsistency(T, T) error
	missingItemError() error
}

type backendAssertionError struct {
	detail string
}

func (e backendAssertionError) Error() string {
	return ErrContractAssertion.Error() + ": " + e.detail
}

func (backendAssertionError) Unwrap() error {
	return ErrContractAssertion
}

func wrapBackendAssertion(err error) error {
	return backendAssertionError{detail: err.Error()}
}

func checkMappedContract[T any](ctx context.Context, fixture Fixture, backend contractBackend[T]) error {
	client := &replayClient{exchanges: fixture.Exchanges, used: make([]bool, len(fixture.Exchanges))}
	provider := backend.provider(client)
	items, err := provider.list(ctx)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrContractAssertion, backend.listOperation(), err)
	}
	item, err := provider.get(ctx)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrContractAssertion, backend.getOperation(), err)
	}
	if err := backend.assertIdentity(item); err != nil {
		return wrapBackendAssertion(err)
	}
	if err := backend.assertRequiredFields(item); err != nil {
		return wrapBackendAssertion(err)
	}
	found := false
	for _, listed := range items {
		if backend.itemID(listed) != backend.targetID() {
			continue
		}
		found = true
		if err := backend.assertConsistency(listed, item); err != nil {
			return wrapBackendAssertion(err)
		}
	}
	if !found {
		return wrapBackendAssertion(backend.missingItemError())
	}
	if err := client.verifyConsumed(); err != nil {
		return fmt.Errorf("%w: %w", ErrContractAssertion, err)
	}
	return nil
}
