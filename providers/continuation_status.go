package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/goobers/goobers/internal/mutationreceipt"
)

type continuationStatus struct {
	ID          int        `json:"id"`
	Context     string     `json:"context"`
	State       string     `json:"status"`
	Description string     `json:"description"`
	TargetURL   string     `json:"target_url"`
	Creator     githubUser `json:"creator"`
}

func (c *restMutationClient) publishStatus(ctx context.Context, req PullRequestStatusRequest) (PullRequestStatusResult, error) {
	if req.Name == "" || req.PullID == "" {
		return PullRequestStatusResult{}, fmt.Errorf("pull id and status name are required")
	}
	head := req.HeadSHA
	if head == "" {
		pull, err := c.pull(ctx, req.Repository, req.PullID)
		if err != nil {
			return PullRequestStatusResult{}, err
		}
		head = pull.Head.SHA
	}
	genre := req.Genre
	if genre == "" {
		genre = "goobers"
	}
	want := map[string]string{"context": genre + "/" + req.Name, "state": giteaStatusState(req.State), "description": req.Description, "target_url": req.TargetURL}
	identity, err := c.identity(ctx, req.Repository, "status-publish", "pulls/"+req.PullID+"/statuses/"+head, want)
	if err != nil {
		return PullRequestStatusResult{}, err
	}
	endpoint, err := joinURL(c.baseURL, "repos", req.Repository.Owner, req.Repository.Name, "statuses", head)
	if err != nil {
		return PullRequestStatusResult{}, err
	}
	result := PullRequestStatusResult{}
	err = c.session.Execute(ctx, identity, func(ctx context.Context, receipts []mutationreceipt.Receipt) (*mutationreceipt.Receipt, error) {
		latest, err := c.latestStatus(ctx, endpoint, want["context"])
		if err != nil || latest == nil {
			return nil, err
		}
		actor, err := c.provider.AuthenticatedLogin(ctx)
		if err != nil {
			return nil, err
		}
		if latest.ID <= 0 || latest.State != want["state"] || latest.Description != req.Description || latest.TargetURL != req.TargetURL || !strings.EqualFold(latest.Creator.Login, actor) {
			return nil, nil
		}
		result.ID = latest.ID
		return &receipts[0], nil
	}, func(ctx context.Context, _ mutationreceipt.Receipt) error {
		if err := c.provider.do(ctx, http.MethodPost, endpoint, want, &result); err != nil {
			return err
		}
		if result.ID <= 0 {
			return fmt.Errorf("%w: status response lacks identity", ErrMutationUnresolved)
		}
		return nil
	})
	return result, err
}

func (c *restMutationClient) latestStatus(ctx context.Context, endpoint, name string) (*continuationStatus, error) {
	var latest *continuationStatus
	err := c.provider.getAllPages(ctx, endpoint, func(page []byte) error {
		var statuses []continuationStatus
		if err := json.Unmarshal(page, &statuses); err != nil {
			return err
		}
		for _, status := range statuses {
			if status.Context != name {
				continue
			}
			if status.ID <= 0 {
				return fmt.Errorf("%w: status evidence lacks identity", ErrMutationUnresolved)
			}
			if latest == nil || status.ID > latest.ID {
				copy := status
				latest = &copy
			}
		}
		return nil
	})
	return latest, err
}
