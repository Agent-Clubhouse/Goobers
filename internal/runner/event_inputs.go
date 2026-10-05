package runner

import (
	"bytes"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/eventing"
	"github.com/goobers/goobers/internal/journal"
)

func prepareEventInputs(in *StartInput, inputs map[string][]byte, grades map[string]apiv1.Integrity, scrubber journal.Scrubber) (*journal.EventLineage, error) {
	if in.EventInputs == nil {
		return nil, nil
	}
	if in.Child != nil {
		return nil, errors.New("runner: event input cannot replace child provenance")
	}
	bundle := in.EventInputs
	manifest, err := bundle.Validate(in.RunID, in.Gaggle)
	if err != nil {
		return nil, err
	}
	start := bundle.Manifest.Start
	if start.ConfigGeneration != in.configGeneration || start.Workflow != in.Machine.Def.Name || start.WorkflowDigest != in.Machine.Digest() || start.GooberDigest != in.GooberDigest || in.Trigger.Kind != journal.TriggerSignal || in.Trigger.Ref != "event:"+start.GroupID {
		return nil, errors.New("runner: event execution pins differ")
	}
	if !bytes.Equal(scrubber.Scrub(manifest), manifest) {
		return nil, errors.New("runner: event manifest requires intake redaction")
	}
	inputs[eventing.ManifestInputName] = manifest
	grades[eventing.ManifestInputName] = apiv1.IntegrityTrusted
	in.ContextPointers = append(in.ContextPointers, eventInputPointer(eventing.ManifestInputName, manifest, apiv1.IntegrityTrusted))
	for i, member := range bundle.Manifest.Members {
		if !member.Selected {
			continue
		}
		raw := bundle.Envelopes[member.ReceiptID]
		if !bytes.Equal(scrubber.Scrub(raw), raw) {
			return nil, errors.New("runner: event payload requires intake redaction")
		}
		name := eventing.EventInputName(i)
		inputs[name] = append([]byte(nil), raw...)
		grades[name] = apiv1.IntegrityUnapproved
		in.ContextPointers = append(in.ContextPointers, eventInputPointer(name, raw, apiv1.IntegrityUnapproved))
	}
	envelope, err := start.Marshal()
	if err != nil {
		return nil, err
	}
	return &journal.EventLineage{Gaggle: in.Gaggle, GroupID: start.GroupID, Consumer: start.Consumer, Revision: start.Revision, AcceptanceID: bundle.Manifest.AcceptanceID, EnvelopeDigest: journal.Digest(envelope), ManifestDigest: journal.Digest(manifest)}, nil
}

func eventInputPointer(name string, raw []byte, grade apiv1.Integrity) apiv1.ContextPointer {
	return apiv1.ContextPointer{Name: name, Integrity: grade, Artifact: &apiv1.ArtifactPointer{Path: "inputs/" + name, Digest: journal.Digest(raw), MediaType: "application/json", Size: int64(len(raw)), Integrity: grade}}
}
