package artifactset

import (
	"fmt"
	"regexp"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// NamedSchemaVersion adds runner-authored named bindings; legacy indexes keep
// their original schema and byte representation.
const NamedSchemaVersion = "goobers.dev/stage-artifact-set/v1alpha2"

// MissingSlotCode reports an omitted required declaration.
const MissingSlotCode = "missing_artifact_slot"

// InvalidPublicationCode reports a refused named publication.
const InvalidPublicationCode = "invalid_artifact_slot_publication"

// SlotBinding identifies one published value without depending on ordinal order.
type SlotBinding struct {
	Stage    string                `json:"stage"`
	Visit    uint64                `json:"visit"`
	Attempt  int32                 `json:"attempt"`
	Slot     string                `json:"slot"`
	Artifact apiv1.ArtifactPointer `json:"artifact"`
}

// PublicationError is a stable producer failure, distinct from an I/O failure.
type PublicationError struct {
	Code string
	Slot string
}

func (e *PublicationError) Error() string { return fmt.Sprintf("%s: slot %q", e.Code, e.Slot) }
func (e *PublicationError) Unwrap() error { return ErrInvalid }

// MissingPublication refuses a declared contract with no manifest. No ordinal
// or generated filename is ever used as a fallback binding.
func MissingPublication(contract *apiv1.ArtifactPublication) error {
	slot := ""
	if contract != nil && len(contract.Slots) > 0 {
		slot = contract.Slots[0].Name
	}
	return &PublicationError{Code: MissingSlotCode, Slot: slot}
}

var slotName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// Bind validates the complete pinned declaration before any bytes are published.
// The manifest supplies workspace requests only; pointers are minted by Publish.
func (p *Prepared) Bind(contract *apiv1.ArtifactPublication, attempt int32) error {
	if contract == nil {
		return nil
	}
	if p == nil || contract.Stage == "" || contract.Visit == 0 || attempt < 1 || len(contract.Slots) == 0 || len(contract.Slots) > MaxEntries {
		return &PublicationError{Code: InvalidPublicationCode}
	}
	seen := map[string]bool{}
	for _, slot := range contract.Slots {
		if !slotName.MatchString(slot.Name) || seen[slot.Name] {
			return &PublicationError{Code: InvalidPublicationCode, Slot: slot.Name}
		}
		seen[slot.Name] = true
		found := false
		for _, entry := range p.entries {
			if entry.name != slot.Name {
				continue
			}
			found = true
			if (slot.MediaType != "" && slot.MediaType != entry.mediaType) || (slot.MaxSize > 0 && int64(len(entry.data)) > slot.MaxSize) {
				return &PublicationError{Code: InvalidPublicationCode, Slot: slot.Name}
			}
		}
		if !found {
			return &PublicationError{Code: MissingSlotCode, Slot: slot.Name}
		}
	}
	copied := *contract
	copied.Slots = append([]apiv1.ArtifactSlot(nil), contract.Slots...)
	p.publication, p.attempt = &copied, attempt
	return nil
}

func validateBindings(index Index, producer string) error {
	if index.SchemaVersion == SchemaVersion {
		if len(index.Bindings) != 0 {
			return fmt.Errorf("%w: legacy index carries named bindings", ErrInvalid)
		}
		return nil
	}
	if len(index.Bindings) == 0 || len(index.Bindings) > MaxEntries {
		return fmt.Errorf("%w: missing named bindings", ErrInvalid)
	}
	previous := ""
	var visit uint64
	var attempt int32
	for _, binding := range index.Bindings {
		if binding.Stage != producer || binding.Visit == 0 || binding.Attempt < 1 || !slotName.MatchString(binding.Slot) || binding.Slot <= previous {
			return fmt.Errorf("%w: invalid named binding identity", ErrInvalid)
		}
		if visit != 0 && (visit != binding.Visit || attempt != binding.Attempt) {
			return fmt.Errorf("%w: mixed producer attempts", ErrInvalid)
		}
		visit, attempt, previous = binding.Visit, binding.Attempt, binding.Slot
		matched := false
		for _, entry := range index.Entries {
			if entry.Name == binding.Slot && entry.Artifact == binding.Artifact {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%w: binding differs from published payload", ErrInvalid)
		}
	}
	return nil
}
