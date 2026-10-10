package readservice

import (
	"context"
	"net/url"

	"github.com/goobers/goobers/internal/triggerqueue"
)

// ChildPublicationSource reads verified local custody only. It cannot contact
// a provider, reconcile an effect, or materialize a publication credential.
type ChildPublicationSource func(context.Context, triggerqueue.ChildIdentity) ([]ChildPublicationObservation, error)

// ChildPublicationObservation binds a safe summary to the source execution.
type ChildPublicationObservation struct {
	SourceRunID string
	Item        ChildPublicationItem
}

// ChildPublicationHistory distinguishes absent effects from unavailable custody.
type ChildPublicationHistory struct {
	Status string                 `json:"status"`
	Items  []ChildPublicationItem `json:"items"`
}

// ChildPublicationItem omits authored PR text, raw intents, artifact references,
// source paths and credentials. It is an observation, not publication authority.
type ChildPublicationItem struct {
	Action            string `json:"action"`
	State             string `json:"state"`
	Head              string `json:"head"`
	Base              string `json:"base"`
	Commit            string `json:"commit"`
	PullRequestURL    string `json:"pullRequestUrl"`
	PullRequestNumber int    `json:"pullRequestNumber"`
	NeedsHuman        bool   `json:"needsHuman"`
}

func (s *Local) childPublications(ctx context.Context, child triggerqueue.ChildRecord) (*ChildPublicationHistory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := &ChildPublicationHistory{Status: "unavailable", Items: []ChildPublicationItem{}}
	if !child.TombstonedAt.IsZero() {
		out.Status = "expired"
		return out, nil
	}
	if s.sources.ChildPublications == nil {
		return out, nil
	}
	statuses, err := s.sources.ChildPublications(ctx, child.Identity)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil || len(statuses) > 2 {
		return out, nil
	}
	seen := map[string]bool{}
	items := make([]ChildPublicationItem, 0, len(statuses))
	for _, observation := range statuses {
		status := observation.Item
		if observation.SourceRunID != child.RunID || (status.Action != "branch" && status.Action != "pr") || seen[status.Action] || !validChildPublication(status) {
			return out, nil
		}
		seen[status.Action] = true
		items = append(items, status)
	}
	out.Status, out.Items = "recorded", items
	return out, nil
}

func validChildPublication(status ChildPublicationItem) bool {
	if status.State != "prepared" && status.State != "effect_pending" && status.State != "confirmed" {
		return false
	}
	if status.NeedsHuman != (status.State == "effect_pending") || len(status.Head) > 1024 || len(status.Base) > 1024 || len(status.Commit) > 128 {
		return false
	}
	if status.Action != "pr" || status.State != "confirmed" {
		return status.PullRequestURL == "" && status.PullRequestNumber == 0
	}
	if status.PullRequestNumber < 1 || len(status.PullRequestURL) > 4096 {
		return false
	}
	link, err := url.Parse(status.PullRequestURL)
	return err == nil && link.Host != "" && link.User == nil && (link.Scheme == "https" || link.Scheme == "http")
}
