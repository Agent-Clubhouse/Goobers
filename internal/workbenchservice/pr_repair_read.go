package workbenchservice

import (
	"context"
	"errors"

	"github.com/goobers/goobers/internal/interactiveaccess"
	"github.com/goobers/goobers/internal/sessioning"
	"github.com/goobers/goobers/internal/triggerqueue"
	"github.com/goobers/goobers/providers"
)

func (s *SessionPRRepair) expected(ctx context.Context, parent string) (string, *providers.RepairPullRequest, error) {
	if parent == "" {
		return s.selection.ExpectedHeadSHA, nil, nil
	}
	record, err := s.service.Queue.PRRepairCommand(ctx, s.scope(), parent)
	if err != nil {
		return "", nil, err
	}
	if record.Input.Selection != s.selection || record.Input.Origin != s.origin || record.State != "confirmed" || record.Receipt == nil || !record.Receipt.ProviderAcknowledged || !record.Receipt.ObservedMatches || record.Input.Target == nil {
		return "", nil, interactiveaccess.ErrDenied
	}
	return record.Receipt.CommitID, record.Input.Target, nil
}
func (s *SessionPRRepair) inspect(ctx context.Context, client PRRepairClient, parent string) (providers.RepairPullRequest, error) {
	head, prior, err := s.expected(ctx, parent)
	if err != nil {
		return providers.RepairPullRequest{}, err
	}
	r := s.selection.Repository
	repository := providers.RepositoryRef{Provider: providers.ProviderKind(r.Provider), Owner: r.Owner, Project: r.Project, Name: r.Name}
	value, err := client.InspectRepairPullRequest(ctx, repository, s.selection.ID)
	if err != nil {
		return value, err
	}
	if value.Repository != repository || value.RepositoryID != s.selection.RepositorySourceID || value.ID != s.selection.ID || value.StableID != s.selection.SourceID || value.HeadSHA != head || !value.Open || (prior != nil && (value.Head != prior.Head || value.Base != prior.Base)) {
		return providers.RepairPullRequest{}, providers.ErrPRRepair
	}
	// Decorative provider text is not needed for immutable effect authority.
	value.Title = string(s.service.Scrubber.Scrub([]byte(value.Title)))
	value.Body = string(s.service.Scrubber.Scrub([]byte(value.Body)))
	if !s.service.exact(value) {
		return providers.RepairPullRequest{}, interactiveaccess.ErrDenied
	}
	return value, nil
}

// Inspect refuses a foreign/current head movement; a confirmed parent command
// is the only way to inspect this turn's own later repair head.
func (s *SessionPRRepair) Inspect(ctx context.Context, parent string) (sessioning.PRRepairInspection, error) {
	var result sessioning.PRRepairInspection
	err := s.use(ctx, func(ctx context.Context, bound ReadBinding) error {
		client, err := s.adapter(ctx, bound)
		if err != nil {
			return err
		}
		value, err := s.inspect(ctx, client, parent)
		if err != nil {
			return err
		}
		result = sessioning.PRRepairInspection{Target: s.selection, ParentCommandID: parent, HeadSHA: value.HeadSHA, BaseSHA: value.BaseSHA, Head: value.Head, Base: value.Base, Title: value.Title, Description: value.Body, URL: value.URL, Open: value.Open, Draft: value.Draft}
		return nil
	})
	return result, prRepairError(err)
}

// ReadFile exposes a bounded exact immutable blob, including proven absence.
func (s *SessionPRRepair) ReadFile(ctx context.Context, path, parent string) (sessioning.PRRepairFile, error) {
	var result sessioning.PRRepairFile
	err := s.use(ctx, func(ctx context.Context, bound ReadBinding) error {
		client, err := s.adapter(ctx, bound)
		if err != nil {
			return err
		}
		target, err := s.inspect(ctx, client, parent)
		if err != nil {
			return err
		}
		file, err := client.ReadRepairFile(ctx, target, path)
		if err != nil {
			return err
		}
		if file.Commit != target.HeadSHA || file.Path != path || !s.service.exact(file) {
			return providers.ErrPRRepair
		}
		result = sessioning.PRRepairFile{Target: s.selection, ParentCommandID: parent, HeadSHA: file.Commit, Path: file.Path, Present: file.Present, BlobID: file.BlobID, Mode: file.Mode, Content: file.Content}
		return nil
	})
	return result, prRepairError(err)
}

// Command is current-actor/current-target receipt access, without token minting
// or revision reads. A confirmed command's original expected head is now stale.
func (s *SessionPRRepair) Command(ctx context.Context, id string) (sessioning.PRRepairCommandView, error) {
	var result sessioning.PRRepairCommandView
	err := s.use(ctx, func(ctx context.Context, _ ReadBinding) error {
		record, err := s.service.Queue.PRRepairCommand(ctx, s.scope(), id)
		if err != nil && !errors.Is(err, triggerqueue.ErrWorkbenchCommandExpired) {
			return err
		}
		target, targetErr := triggerqueue.PRRepairTargetDigest(s.scope(), s.selection)
		if targetErr != nil || target != record.Input.TargetDigest {
			return interactiveaccess.ErrDenied
		}
		if err != nil {
			return err
		}
		if record.Input.Selection != s.selection || record.Input.Origin != s.origin {
			return interactiveaccess.ErrDenied
		}
		result = prRepairView(record)
		return nil
	})
	return result, prRepairError(err)
}
