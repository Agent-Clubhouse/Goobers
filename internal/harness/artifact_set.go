package harness

import (
	"context"
	"fmt"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
)

// InputArtifactManifestFile requests runner-authored multi-file lifting. It is
// mutually exclusive with artifactFile and forbids self-reported pointers.
const InputArtifactManifestFile = "artifactManifestFile"

func (e *Executor) liftArtifacts(ctx context.Context, env apiv1.InvocationEnvelope, reported []apiv1.ArtifactPointer) ([]apiv1.ArtifactPointer, error) {
	value, manifestMode := env.Inputs[InputArtifactManifestFile]
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
	prepared, err := artifactset.Prepare(ctx, env.Workspace, manifest, artifactset.NewSanitizer(e.scrubber))
	if err != nil {
		return nil, err
	}
	return prepared.Publish(ctx, func(name, media string, data []byte) (apiv1.ArtifactPointer, error) {
		ref, err := e.artifacts.RecordArtifact(env.TaskID+"/"+name, data)
		if err != nil {
			return apiv1.ArtifactPointer{}, err
		}
		return refToPointer(ref, media), nil
	})
}
