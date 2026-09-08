package providers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// LandingIntent is a durable attempt, never proof that the forge merged a PR.
// Recovery must retain that distinction even when it later observes a merge.
type LandingIntent struct {
	ID               string `json:"id"`
	Operation        string `json:"operation,omitempty"`
	RepositoryAPIURL string `json:"repositoryApiUrl"`
	PullID           string `json:"pullId"`
	ExpectedHeadSHA  string `json:"expectedHeadSha,omitempty"`
}

// LandingIntentRecorder acknowledges durable storage before a landing request.
// Existing observation-only recorders remain valid for non-durable clients.
type LandingIntentRecorder interface {
	RecordLandingIntent(context.Context, ProviderKind, LandingIntent) error
}

// LandingReceiptRecorder acknowledges durable storage after a successful
// external operation. A receipt failure does not undo that operation.
type LandingReceiptRecorder interface {
	RecordLandingReceipt(context.Context, ExternalRef) error
}

// LandingReceiptError means the external operation succeeded but its receipt
// is not durably acknowledged. Callers must not describe it as a forge refusal.
type LandingReceiptError struct{ Cause error }

func (e *LandingReceiptError) Error() string {
	return "landing succeeded but durable receipt persistence failed"
}
func (e *LandingReceiptError) Unwrap() error { return e.Cause }

func recordLandingReceipt(ctx context.Context, recorder MutationRecorder, ref ExternalRef) error {
	if durable, ok := recorder.(LandingReceiptRecorder); ok {
		if err := durable.RecordLandingReceipt(ctx, ref); err != nil {
			return &LandingReceiptError{Cause: err}
		}
		return nil
	}
	if recorder != nil {
		recorder.RecordExternalRef(ctx, ref)
	}
	return nil
}

func prepareLandingIntent(ctx context.Context, recorder MutationRecorder, provider ProviderKind, repository, pullID, head, operation string) (*LandingIntent, error) {
	durable, ok := recorder.(LandingIntentRecorder)
	if !ok {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	canonical := newMergeConfirmation(repository, pullID, "")
	if canonical == nil {
		return nil, fmt.Errorf("landing intent: invalid repository route")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("landing intent identity: %w", err)
	}
	if operation != "merge" && operation != "enqueue" {
		return nil, fmt.Errorf("landing intent: invalid operation %q", operation)
	}
	intent := LandingIntent{ID: hex.EncodeToString(nonce[:]), Operation: operation, RepositoryAPIURL: canonical.RepositoryAPIURL, PullID: pullID, ExpectedHeadSHA: head}
	if err := durable.RecordLandingIntent(ctx, provider, intent); err != nil {
		return nil, fmt.Errorf("persist landing intent: %w", err)
	}
	return &intent, nil
}
