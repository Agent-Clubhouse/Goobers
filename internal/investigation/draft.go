package investigation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
)

// DraftSchemaVersion marks an agent-authored manifest whose artifact positions
// contain only {producerStage,name}, never agent-authored artifact pointers.
const DraftSchemaVersion = "goobers.dev/investigation-evidence-draft/v1alpha1"

// PrepareDraft resolves every semantic reference through complete verified
// upstream indices, then prepares canonical evidence without publishing it.
// The caller must finish validating its entire artifact set before publication.
func PrepareDraft(ctx context.Context, data []byte, pointers []apiv1.ContextPointer, reader artifactset.Reader, scrubber artifactset.Scrubber) ([]byte, error) {
	if len(data) > maxManifestBytes || len(pointers) > 4096 {
		return nil, errors.New("investigation: draft size limit")
	}
	clean, err := artifactset.NewSanitizer(scrubber)("application/json", data)
	if err != nil {
		return nil, err
	}
	schema, err := draftSchema()
	if err != nil {
		return nil, err
	}
	if err := validateSchemaJSON(schema, clean); err != nil {
		return nil, errors.New("investigation: draft violates the versioned schema")
	}
	decoder := json.NewDecoder(bytes.NewReader(clean))
	decoder.UseNumber()
	var draft map[string]any
	if err := decoder.Decode(&draft); err != nil || draft["schemaVersion"] != DraftSchemaVersion {
		return nil, errors.New("investigation: invalid draft version or shape")
	}
	resolve := &draftResolver{ctx: ctx, pointers: pointers, reader: reader, cache: map[string]map[string]apiv1.ArtifactPointer{}}
	for _, path := range [][2]string{{"reproduction", "harness"}, {"reproduction", "baseline"}, {"diagnosis", "report"}, {"fix", "report"}, {"validation", "result"}} {
		section, ok := draft[path[0]].(map[string]any)
		if !ok {
			return nil, errors.New("investigation: missing draft section")
		}
		if err := resolve.replace(section, path[1]); err != nil {
			return nil, err
		}
	}
	diagnosis := draft["diagnosis"].(map[string]any)
	if err := resolve.list(diagnosis["evidence"]); err != nil {
		return nil, err
	}
	if attachments, exists := draft["attachments"]; exists {
		if err := resolve.list(attachments); err != nil {
			return nil, err
		}
	}
	draft["schemaVersion"] = SchemaVersion
	canonical, err := json.Marshal(draft)
	if err != nil {
		return nil, err
	}
	// Validate before decoding to a struct, so unknown draft fields cannot be
	// silently dropped by typed JSON unmarshalling.
	if err := validateEvidenceJSON(canonical); err != nil {
		return nil, errors.New("investigation: resolved draft violates evidence schema")
	}
	var evidence Evidence
	if err := json.Unmarshal(canonical, &evidence); err != nil {
		return nil, errors.New("investigation: invalid resolved draft")
	}
	return prepareEvidence(ctx, evidence, pointers, reader, scrubber)
}

type draftResolver struct {
	ctx      context.Context
	pointers []apiv1.ContextPointer
	reader   artifactset.Reader
	cache    map[string]map[string]apiv1.ArtifactPointer
}

func (r *draftResolver) replace(object map[string]any, key string) error {
	ref, ok := object[key].(map[string]any)
	if !ok || len(ref) != 2 {
		return errors.New("investigation: expected semantic reference, not artifact pointer")
	}
	stage, stageOK := ref["producerStage"].(string)
	name, nameOK := ref["name"].(string)
	if !stageOK || !nameOK || name == "" {
		return errors.New("investigation: invalid semantic reference")
	}
	switch stage {
	case "reproduce", "instrument", "implement-fix", "validate-reproduction":
	default:
		return errors.New("investigation: invalid producer stage")
	}
	if r.cache[stage] == nil {
		payloads, err := artifactset.Resolve(r.ctx, r.reader, r.pointers, stage)
		if err != nil {
			return err
		}
		r.cache[stage] = make(map[string]apiv1.ArtifactPointer, len(payloads))
		for name, payload := range payloads {
			r.cache[stage][name] = payload.Artifact
		}
	}
	pointer, found := r.cache[stage][name]
	if !found {
		return errors.New("investigation: missing semantic artifact")
	}
	object[key] = pointer
	return nil
}

func (r *draftResolver) list(value any) error {
	list, ok := value.([]any)
	if !ok || len(list) > artifactset.MaxEntries {
		return errors.New("investigation: invalid evidence list")
	}
	for _, value := range list {
		entry, ok := value.(map[string]any)
		if !ok {
			return errors.New("investigation: invalid evidence entry")
		}
		if err := r.replace(entry, "artifact"); err != nil {
			return err
		}
	}
	return nil
}
