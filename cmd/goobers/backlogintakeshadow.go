package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/providers"
)

const backlogIntakeShadowAnnotation = "backlog.intake-decision-shadow"
const backlogIntakeShadowTimeout = 20 * time.Second

type backlogIntakeJudge interface {
	EvaluateIntakeRisk(context.Context, decisiongate.IntakeItem, []decisiongate.IntakeItem) (decisiongate.Outcome, error)
}

var resolveBacklogIntakeJudge = func(layout instance.Layout) (backlogIntakeJudge, float64, bool, error) {
	cfg, err := instance.LoadConfig(layout.ConfigFile())
	if err != nil {
		return nil, 0, false, err
	}
	if cfg.DecisionGate.EffectiveMode() != decisiongate.ModeShadow {
		return nil, 0, false, nil
	}
	gate, err := cfg.DecisionGate.Resolve(nil, nil)
	if err != nil {
		return nil, 0, false, err
	}
	return gate, cfg.DecisionGate.ShadowSample, true, nil
}

func observeBacklogIntakeShadow(
	ctx context.Context,
	env backlogQueryEnv,
	runID, workflow string,
	trustLabel string,
	eligible, peers []providers.WorkItem,
) {
	judge, sample, enabled, err := resolveBacklogIntakeJudge(env.layout)
	if err != nil {
		pf(env.stderr, "warning: decisionGate backlog intake shadow disabled: %v\n", err)
		return
	}
	if !enabled || len(eligible) == 0 {
		return
	}
	if trustLabel != "" && env.issueProvider != nil {
		listed, listErr := env.issueProvider.ListWorkItems(ctx, providers.ListWorkItemsRequest{
			Repository:  env.backlogRepo,
			Labels:      []string{trustLabel},
			State:       "open",
			Limit:       backlogScanCeiling,
			OldestFirst: true,
		})
		if listErr != nil {
			pf(env.stderr, "warning: decisionGate backlog intake shadow comparison scan failed: %v\n", listErr)
		} else {
			peers = peers[:0]
			for _, item := range listed {
				if item.HasLabel(trustLabel) {
					peers = append(peers, item)
				}
			}
		}
	}
	annotations, err := openStageAnnotator(env.layout)
	if err != nil {
		pf(env.stderr, "warning: decisionGate backlog intake shadow annotations disabled: %v\n", err)
		return
	}
	defer func() {
		if closeErr := annotations.Close(); closeErr != nil {
			pf(env.stderr, "warning: close decisionGate backlog intake shadow annotations: %v\n", closeErr)
		}
	}()

	open := make([]decisiongate.IntakeItem, 0, len(peers))
	for _, item := range peers {
		open = append(open, decisiongate.IntakeItem{ID: item.ID, Title: item.Title, Body: item.Body})
	}
	type shadowResult struct {
		item    providers.WorkItem
		outcome decisiongate.Outcome
		err     error
	}
	results := make([]shadowResult, 0, len(eligible))
	for _, item := range eligible {
		repo := env.issueRepo()
		sampleKey := fmt.Sprintf("%s/%s/%s/%s/%s", repo.Provider, repo.Owner, repo.Project, repo.Name, item.ID)
		if !decisiongate.Sampled(sampleKey, sample) {
			continue
		}
		results = append(results, shadowResult{item: item})
	}
	shadowCtx, cancel := context.WithTimeout(ctx, backlogIntakeShadowTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(result *shadowResult) {
			defer wg.Done()
			result.outcome, result.err = judge.EvaluateIntakeRisk(shadowCtx, decisiongate.IntakeItem{
				ID: result.item.ID, Title: result.item.Title, Body: result.item.Body,
			}, open)
		}(&results[i])
	}
	wg.Wait()
	repo := env.issueRepo()
	for _, result := range results {
		item, outcome, judgeErr := result.item, result.outcome, result.err
		peerCount := len(open)
		for _, peer := range open {
			if peer.ID == item.ID {
				peerCount--
				break
			}
		}
		record := journal.Event{
			Type:     journal.EventRunnerAnnotation,
			RunID:    runID,
			Workflow: workflow,
			Runner: map[string]any{
				"annotation":    backlogIntakeShadowAnnotation,
				"itemId":        item.ID,
				"provider":      string(repo.Provider),
				"repositoryKey": repo.CanonicalKey(),
				"verdict":       string(outcome.Decision),
				"flagged":       outcome.Decision == decisiongate.Yes,
				"probability":   outcome.Probability,
				"cached":        outcome.Cached,
				"error":         judgeErr != nil,
				"peerCount":     peerCount,
				"shadowSample":  sample,
			},
		}
		if err := annotations.Append(record); err != nil {
			pf(env.stderr, "warning: record decisionGate backlog intake shadow for item %s: %v\n", item.ID, err)
		}
	}
}
