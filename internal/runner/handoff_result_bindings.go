package runner

import (
	"context"
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/artifactset"
	"github.com/goobers/goobers/internal/workflow"
)

// ResultHandoffSchemas binds a producer stage's sole JSON artifact (for a
// deterministic stage, its `.json` resultFile) to a trusted JSON Schema path,
// keyed by workflow name and then stage name. Workflows that predate DSL 3.1
// cannot declare schema-bound artifactSlots, so without a binding every
// consumer's handoff validity stays unknown (#6733). Nil preserves historical
// behavior; bindings are inert unless HandoffSchemaLoader is also set.
type ResultHandoffSchemas map[string]map[string]string

const resultHandoffSlot = "result"

func (s ResultHandoffSchemas) forWorkflow(machine *workflow.Machine) map[string]string {
	if machine == nil {
		return nil
	}
	return s[machine.Def.Name]
}

// resultHandoffBinding binds task's result artifact when an operator schema is
// configured. A stage that declares artifactSlots publishes a named set
// instead, so its slots keep the authoritative binding.
func resultHandoffBinding(task apiv1.Task, schemaPath string) (handoffBinding, bool) {
	if schemaPath == "" || len(task.ArtifactSlots) > 0 {
		return handoffBinding{}, false
	}
	return handoffBinding{
		LocalName:      task.Name + "." + resultHandoffSlot,
		ProducerTask:   task.Name,
		SlotName:       resultHandoffSlot,
		MediaType:      "application/json",
		SchemaPath:     schemaPath,
		ResultArtifact: true,
	}, true
}

// readResultHandoff reads producer's sole current-run JSON artifact with
// digest and size verification. Zero or several candidates are ambiguous and
// must never yield an authoritative verdict.
func readResultHandoff(ctx context.Context, reader *artifactset.JournalReader, pointers []apiv1.ContextPointer, producer string) ([]byte, error) {
	var found *apiv1.ArtifactPointer
	for _, pointer := range pointers {
		if pointer.Artifact == nil || pointer.External != nil || pointer.RunID != "" || pointer.Artifact.MediaType != "application/json" {
			continue
		}
		if name, _, ok := strings.Cut(pointer.Name, ".artifact["); !ok || name != producer {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("producer %q has more than one JSON artifact", producer)
		}
		found = pointer.Artifact
	}
	if found == nil {
		return nil, fmt.Errorf("producer %q has no JSON artifact", producer)
	}
	return reader.ReadArtifact(ctx, *found, artifactset.MaxPayloadBytes)
}
