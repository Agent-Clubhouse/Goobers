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
	RepositoryAPIURL string `json:"repositoryApiUrl"`
	PullID           string `json:"pullId"`
	ExpectedHeadSHA  string `json:"expectedHeadSha,omitempty"`
}

// LandingIntentRecorder acknowledges durable storage before a landing request.
// Existing observation-only recorders remain valid for non-durable clients.
type LandingIntentRecorder interface {
	RecordLandingIntent(context.Context, ProviderKind, LandingIntent) error
}

func prepareLandingIntent(ctx context.Context, recorder MutationRecorder, provider ProviderKind, repository, pullID, head string) (*LandingIntent, error) {
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
	intent := LandingIntent{ID: hex.EncodeToString(nonce[:]), RepositoryAPIURL: canonical.RepositoryAPIURL, PullID: pullID, ExpectedHeadSHA: head}
	if err := durable.RecordLandingIntent(ctx, provider, intent); err != nil {
		return nil, fmt.Errorf("persist landing intent: %w", err)
	}
	return &intent, nil
}
