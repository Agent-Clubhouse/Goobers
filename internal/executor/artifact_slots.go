package executor

import (
	"context"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/journal"
)

// publishArtifactSlots uses the same contained, sanitized publication boundary
// as harness output. Existing stdout/stderr/result artifacts remain available.
func (e *ShellExecutor) publishArtifactSlots(ctx context.Context, env apiv1.InvocationEnvelope, result *apiv1.ResultEnvelope, scrubber journal.Scrubber) error {
	manifest, ok := env.Inputs["artifactManifestFile"].(string)
	var err error
	var pointers []apiv1.ArtifactPointer
	if !ok || manifest == "" {
		err = artifactset.MissingPublication(env.ArtifactPublication)
	} else {
		var prepared *artifactset.Prepared
		prepared, err = artifactset.Prepare(ctx, env.Workspace, manifest, artifactset.NewSanitizer(scrubber))
		if err == nil {
			err = prepared.Bind(env.ArtifactPublication, env.Attempt)
		}
		if err == nil {
			pointers, err = prepared.Publish(ctx, func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
				ref, recordErr := e.recordPreparedSlot(ctx, env.TaskID+"/"+name, media, data)
				return refToPointer(ref, media), recordErr
			})
		}
	}
	if err == nil {
		result.Artifacts = append(pointers, result.Artifacts...)
		return nil
	}
	var publication *artifactset.PublicationError
	if errors.As(err, &publication) {
		result.Status, result.Summary = apiv1.ResultFailure, publication.Error()
		result.Error = journal.ErrorInfoFor(publication.Code, err, false)
		return nil
	}
	if errors.Is(err, artifactset.ErrInvalid) {
		result.Status, result.Summary = apiv1.ResultFailure, "declared artifact set is invalid"
		result.Error = journal.ErrorInfoFor("invalid_declared_artifact_set", err, false)
		return nil
	}
	return err
}

func publicationScrubber(ctx context.Context) (*journal.RegistryScrubber, journal.Scrubber) {
	registry, scrubber := journal.DefaultScrubber()
	registerJournalPlane(ctx, registry)
	return registry, scrubber
}

func (e *ShellExecutor) publishNamedResult(ctx context.Context, env apiv1.InvocationEnvelope, scrubber journal.Scrubber, result *apiv1.ResultEnvelope, err *error) {
	if env.ArtifactPublication != nil && *err == nil && result.Status != "" {
		*err = e.publishArtifactSlots(ctx, env, result, scrubber)
	}
}

// recordPreparedSlot uses the same durable recorder capability as the harness;
// a remote pod must await verified adoption rather than publish a diagnostic.
func (e *ShellExecutor) recordPreparedSlot(ctx context.Context, name, media string, data []byte) (journal.Ref, error) {
	if recorder, ok := e.Journal.(interface {
		RecordPreparedArtifact(context.Context, string, string, []byte) (journal.Ref, error)
	}); ok {
		return recorder.RecordPreparedArtifact(ctx, name, media, data)
	}
	return e.Journal.RecordArtifact(name, data)
}
