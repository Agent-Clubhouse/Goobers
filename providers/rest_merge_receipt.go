package providers

import (
	"context"
	"strconv"
	"strings"
)

type restMergeReceipt struct {
	recorder         MutationRecorder
	provider         ProviderKind
	repository       RepositoryRef
	repositoryAPIURL string
	pullID           string
	number           int
	intent           *LandingIntent
}

func prepareRESTMergeReceipt(ctx context.Context, recorder MutationRecorder, provider ProviderKind, repository RepositoryRef, baseURL, pullID, expectedHeadSHA string) (restMergeReceipt, error) {
	repositoryAPIURL, _ := joinURL(baseURL, "repos", strings.ToLower(repository.Owner), strings.ToLower(repository.Name))
	intent, err := prepareLandingIntent(ctx, recorder, provider, repositoryAPIURL, pullID, expectedHeadSHA, "merge")
	if err != nil {
		return restMergeReceipt{}, err
	}
	number, _ := strconv.Atoi(pullID)
	return restMergeReceipt{
		recorder:         recorder,
		provider:         provider,
		repository:       repository,
		repositoryAPIURL: repositoryAPIURL,
		pullID:           pullID,
		number:           number,
		intent:           intent,
	}, nil
}

func (r restMergeReceipt) record(ctx context.Context, mergeSHA string, merged bool) error {
	if !merged {
		return nil
	}
	confirmation := newMergeConfirmation(r.repositoryAPIURL, r.pullID, mergeSHA)
	if r.intent != nil {
		confirmation.IntentID = r.intent.ID
	}
	return recordLandingReceipt(ctx, r.recorder, ExternalRef{
		MergeConfirmation: confirmation,
		Provider:          r.provider,
		Ref:               issueRef(r.repository, r.pullID),
		Operation:         "merge",
		Fields:            map[string]FieldDigest{"state": {After: digestString("merged")}},
	})
}
