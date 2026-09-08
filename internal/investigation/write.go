package investigation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
)

const maxManifestBytes = 256 << 10

// prepareEvidence verifies references and prepares bytes without publishing.
// Publication belongs to the executor's whole-set artifact boundary.
func prepareEvidence(ctx context.Context, evidence Evidence, pointers []apiv1.ContextPointer, reader artifactset.Reader, scrubber artifactset.Scrubber) ([]byte, error) {
	if reader == nil || scrubber == nil {
		return nil, errors.New("investigation: missing evidence preparation dependency")
	}
	data, err := json.Marshal(evidence)
	if err != nil {
		return nil, errors.New("investigation: evidence is not JSON-encodable")
	}
	if len(data) > maxManifestBytes {
		return nil, errors.New("investigation: manifest byte limit")
	}
	clean, err := artifactset.NewSanitizer(scrubber)("application/json", data)
	if err != nil {
		return nil, errors.New("investigation: unsafe evidence JSON")
	}
	if len(clean) > maxManifestBytes {
		return nil, errors.New("investigation: redacted manifest byte limit")
	}
	if err := validateEvidenceJSON(clean); err != nil {
		return nil, errors.New("investigation: evidence violates the versioned schema")
	}
	var canonical Evidence
	if err := json.Unmarshal(clean, &canonical); err != nil {
		return nil, errors.New("investigation: redacted manifest is invalid")
	}
	if err := validateMetadata(canonical); err != nil {
		return nil, err
	}
	if err := verifyReferences(ctx, canonical, pointers, reader); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return clean, nil
}

func verifyReferences(ctx context.Context, evidence Evidence, pointers []apiv1.ContextPointer, reader artifactset.Reader) error {
	if len(pointers) > 4096 {
		return fmt.Errorf("%w: context count", artifactset.ErrInvalid)
	}
	allowed := make(map[producedReference]bool)
	for _, pointer := range pointers {
		if pointer.Artifact != nil && pointer.External == nil && pointer.RunID == "" && pointer.Branch == 0 && pointer.BranchName == "" {
			if producer := artifactProducer(pointer.Name); producer != "" {
				allowed[producedReference{*pointer.Artifact, producer}] = true
			}
		}
	}
	refs := []producedReference{{evidence.Reproduction.Harness, "reproduce"}, {evidence.Reproduction.Baseline, "reproduce"}, {evidence.Diagnosis.Report, "instrument"}, {evidence.Fix.Report, "implement-fix"}, {evidence.Validation.Result, "validate-reproduction"}}
	for _, ref := range evidence.Diagnosis.Evidence {
		refs = append(refs, producedReference{ref.Artifact, ref.ProducerStage})
	}
	for _, ref := range evidence.Attachments {
		refs = append(refs, producedReference{ref.Artifact, ref.ProducerStage})
	}
	seen := make(map[apiv1.ArtifactPointer]bool)
	var total int64
	for _, ref := range refs {
		pointer := ref.pointer
		if err := pointer.Validate(); err != nil {
			return fmt.Errorf("%w: malformed evidence pointer", artifactset.ErrInvalid)
		}
		if !allowed[ref] {
			return fmt.Errorf("%w: reference is not current-run evidence", artifactset.ErrInvalid)
		}
		if seen[pointer] {
			continue
		}
		seen[pointer] = true
		if pointer.Size < 0 || pointer.Size > artifactset.MaxPayloadBytes {
			return fmt.Errorf("%w: evidence payload byte limit", artifactset.ErrInvalid)
		}
		total += pointer.Size
		if total > artifactset.MaxSetBytes {
			return fmt.Errorf("%w: evidence set byte limit", artifactset.ErrInvalid)
		}
		data, err := reader.ReadArtifact(ctx, pointer, artifactset.MaxPayloadBytes)
		if err != nil {
			return err
		}
		if int64(len(data)) != pointer.Size || apiv1.Digest(data) != pointer.Digest {
			return fmt.Errorf("%w: evidence content mismatch", artifactset.ErrInvalid)
		}
	}
	return nil
}

type producedReference struct {
	pointer apiv1.ArtifactPointer
	stage   string
}

func artifactProducer(name string) string {
	producer, slot, found := strings.Cut(name, ".artifact[")
	if !found || !strings.HasSuffix(slot, "]") {
		return ""
	}
	number, err := strconv.Atoi(strings.TrimSuffix(slot, "]"))
	if err != nil || number < 0 {
		return ""
	}
	return producer
}
