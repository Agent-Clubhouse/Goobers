package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/handoffcheck"
	"github.com/goobers/goobers/internal/investigation"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
)

// InputArtifactManifestFile requests runner-authored multi-file lifting. It is
// mutually exclusive with artifactFile and forbids self-reported pointers.
const InputArtifactManifestFile = "artifactManifestFile"

// PreparedArtifactRecorder is an optional durable path for recorders whose
// ordinary diagnostics are best effort. Data has already passed the complete
// artifact-set validation and media-aware sanitizer. Implementations preserve
// those exact bytes and return only after durable publication succeeds.
type PreparedArtifactRecorder interface {
	RecordPreparedArtifact(ctx context.Context, name, mediaType string, data []byte) (journal.Ref, error)
}

func (e *Executor) liftArtifacts(ctx context.Context, env apiv1.InvocationEnvelope, reported []apiv1.ArtifactPointer) ([]apiv1.ArtifactPointer, error) {
	value, manifestMode := env.Inputs[InputArtifactManifestFile]
	if env.ArtifactPublication != nil {
		if len(reported) != 0 {
			return nil, &artifactset.PublicationError{Code: artifactset.InvalidPublicationCode}
		}
		if !manifestMode {
			return nil, artifactset.MissingPublication(env.ArtifactPublication)
		}
	}
	if !manifestMode {
		pointer, err := e.liftArtifactFile(env)
		if err != nil {
			return nil, err
		}
		if pointer != nil {
			reported = append(reported, *pointer)
		}
		return reported, nil
	}
	manifest, ok := value.(string)
	_, legacy := env.Inputs[InputArtifactFile]
	if !ok || manifest == "" || legacy || len(reported) != 0 {
		return nil, fmt.Errorf("%w: manifest mode requires a path, no artifactFile, and no self-reported pointers", artifactset.ErrInvalid)
	}
	prepared, err := e.prepareArtifactSet(ctx, env, manifest)
	if err != nil {
		return nil, err
	}
	if err := prepared.Bind(env.ArtifactPublication, env.Attempt); err != nil {
		return nil, err
	}
	if err := prepared.CheckSchemas(declaredPublicationSchemas(ctx, env)); err != nil {
		return nil, err
	}
	return prepared.Publish(ctx, func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
		ref, err := e.recordPreparedArtifact(ctx, env.TaskID+"/"+name, media, data)
		if err != nil {
			return apiv1.ArtifactPointer{}, err
		}
		return refToPointer(ref, media), nil
	})
}

func (e *Executor) recordPreparedArtifact(ctx context.Context, name, media string, data []byte) (journal.Ref, error) {
	if recorder, ok := e.artifacts.(PreparedArtifactRecorder); ok {
		return recorder.RecordPreparedArtifact(ctx, name, media, data)
	}
	return e.artifacts.RecordArtifact(name, data)
}

func (e *Executor) prepareArtifactSet(ctx context.Context, env apiv1.InvocationEnvelope, manifest string) (prepared *artifactset.Prepared, retErr error) {
	var reader *artifactset.JournalReader
	defer func() {
		if reader != nil {
			if err := reader.Close(); err != nil {
				prepared = nil
				retErr = errors.Join(retErr, err)
			}
		}
	}()
	sanitize := artifactset.NewSanitizer(e.scrubber)
	return artifactset.Prepare(ctx, env.Workspace, manifest, func(media string, data []byte) ([]byte, error) {
		clean, err := sanitize(media, data)
		if err != nil || media != "application/json" {
			return clean, err
		}
		var header struct {
			SchemaVersion string `json:"schemaVersion"`
		}
		if err := json.Unmarshal(clean, &header); err != nil {
			return clean, nil // Other JSON shapes remain ordinary payloads.
		}
		if header.SchemaVersion == investigation.SchemaVersion {
			return nil, errors.New("canonical investigation pointers must be runner-authored")
		}
		if header.SchemaVersion != investigation.DraftSchemaVersion {
			return clean, nil
		}
		if reader == nil {
			reader, err = artifactset.OpenJournal(e.contextResolver.Dir())
			if err != nil {
				return nil, err
			}
		}
		return investigation.PrepareDraft(ctx, clean, env.ContextPointers, reader, e.scrubber)
	})
}

// declaredPublicationSchemas narrows the runner-supplied producer schemas to
// the slots pinned on this invocation's publication contract, so a schema can
// only constrain a slot this stage is actually required to publish.
func declaredPublicationSchemas(ctx context.Context, env apiv1.InvocationEnvelope) map[string]*handoffcheck.Schema {
	all := handoffcheck.PublicationSchemasFromContext(ctx)
	if len(all) == 0 || env.ArtifactPublication == nil {
		return nil
	}
	if _, manifestMode := env.Inputs[InputArtifactManifestFile]; !manifestMode {
		return nil
	}
	var out map[string]*handoffcheck.Schema
	for _, slot := range env.ArtifactPublication.Slots {
		schema, ok := all[slot.Name]
		if !ok {
			continue
		}
		if out == nil {
			out = map[string]*handoffcheck.Schema{}
		}
		out[slot.Name] = schema
	}
	return out
}

// publicationSchemasFor hands the declared schemas to goobers-io so
// publish_output rejects a nonconforming payload while the session can still
// repair it (#6868).
func publicationSchemasFor(ctx context.Context, env apiv1.InvocationEnvelope) []mcpio.PublicationSchema {
	schemas := declaredPublicationSchemas(ctx, env)
	if len(schemas) == 0 {
		return nil
	}
	slots := make([]string, 0, len(schemas))
	for slot := range schemas {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	out := make([]mcpio.PublicationSchema, 0, len(slots))
	for _, slot := range slots {
		out = append(out, mcpio.PublicationSchema{Slot: slot, SchemaID: schemas[slot].ID(), Document: schemas[slot].Source()})
	}
	return out
}
