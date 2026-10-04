package interactivesession

import (
	"context"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/sessioning"
)

// Get returns a summary only after explicit gaggle viewer authorization.
func (s *Service) Get(ctx context.Context, p httpapi.Principal, gaggle, id string) (result sessioning.Session, err error) {
	defer func() { err = apiError(err) }()
	if err := s.ready(); err != nil {
		return result, err
	}
	err = s.Permissions.WithSessionView(ctx, p, gaggle, func(ctx context.Context) error {
		var err error
		result, err = s.Queue.Session(ctx, gaggle, id)
		return err
	})
	if err == nil {
		result.Title, err = s.clean(result.Title, sessioning.MaxTitleBytes, false)
	}
	return result, mapped(err)
}

// List pages shared sessions without materializing their conversations.
func (s *Service) List(ctx context.Context, p httpapi.Principal, gaggle, cursor string, limit int) (result sessioning.SessionPage, err error) {
	defer func() { err = apiError(err) }()
	if err := s.ready(); err != nil {
		return result, err
	}
	err = s.Permissions.WithSessionView(ctx, p, gaggle, func(ctx context.Context) error {
		var err error
		result, err = s.Queue.Sessions(ctx, gaggle, cursor, limit)
		return err
	})
	if err == nil {
		for i := range result.Items {
			result.Items[i].Title, err = s.clean(result.Items[i].Title, sessioning.MaxTitleBytes, false)
			if err != nil {
				break
			}
		}
	}
	return result, mapped(err)
}

// Messages re-scrubs output against credentials registered since acceptance.
func (s *Service) Messages(ctx context.Context, p httpapi.Principal, gaggle, id string, after uint64, limit int) (result sessioning.MessagePage, err error) {
	defer func() { err = apiError(err) }()
	if err := s.ready(); err != nil {
		return result, err
	}
	err = s.Permissions.WithSessionView(ctx, p, gaggle, func(ctx context.Context) error {
		var err error
		result, err = s.Queue.SessionMessages(ctx, gaggle, id, after, limit)
		return err
	})
	if err != nil {
		return result, mapped(err)
	}
	for i := range result.Items {
		text, err := s.clean(result.Items[i].Text, sessioning.MaxTextBytes, false)
		if err != nil {
			return sessioning.MessagePage{}, err
		}
		result.Items[i].Text = text
		if !s.safeRepairTarget(result.Items[i].RepairTarget) {
			result.Items[i].RepairTarget = nil
		}
	}
	return result, nil
}
