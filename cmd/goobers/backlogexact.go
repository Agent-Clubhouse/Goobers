package main

import (
	"context"
	"errors"
	"os"
	"strings"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/internal/workitemtarget"
	"github.com/goobers/goobers/providers"
)

func exactBacklogWorkItem() (string, error) {
	ref := os.Getenv(executor.TriggerRefEnvVar)
	if !strings.HasPrefix(ref, "work-item:") {
		return "", nil
	}
	id := strings.TrimPrefix(ref, "work-item:")
	if !workitemtarget.ValidID(id) {
		return "", errors.New("invalid exact work item trigger reference")
	}
	return id, nil
}

func getExactBacklogWorkItem(ctx context.Context, env backlogQueryEnv, id string) ([]providers.WorkItem, error) {
	item, err := env.issueProvider.GetWorkItem(ctx, env.backlogRepo, id)
	if err != nil {
		return nil, err
	}
	if item.ID != id {
		return nil, errors.New("provider returned a different exact work item")
	}
	return []providers.WorkItem{item}, nil
}

func restrictExactBacklogWorkItem(items []providers.WorkItem, id string) ([]providers.WorkItem, error) {
	if id == "" {
		return items, nil
	}
	for _, item := range items {
		if item.ID == id {
			return []providers.WorkItem{item}, nil
		}
	}
	return nil, errors.New("the exact requested work item is not eligible; no other item may be claimed")
}
