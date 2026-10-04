package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/journal"
)

const childCredentialInput = "child-credential-ceiling"
const maxChildCredentialBytes = 32 << 10

type childCredentialRecord struct {
	SourceDigest   string                   `json:"sourceDigest"`
	EnvelopeDigest string                   `json:"envelopeDigest"`
	Ceiling        credentials.ChildCeiling `json:"ceiling"`
}

func pinChildCredentials(in *StartInput, inputs map[string][]byte, integrity map[string]apiv1.Integrity) error {
	if in.Child == nil {
		if in.ChildCredentials != nil {
			return errors.New("runner: child credential ceiling requires child provenance")
		}
		return nil
	}
	ceiling := credentials.NewChildCeiling(false, nil, nil)
	if in.ChildCredentials != nil {
		ceiling = *in.ChildCredentials
	}
	if err := ceiling.Validate(); err != nil {
		return err
	}
	record := childCredentialRecord{SourceDigest: in.Child.SourceDigest, EnvelopeDigest: in.Child.EnvelopeDigest, Ceiling: ceiling}
	data, err := json.Marshal(record)
	if err != nil || len(data) > maxChildCredentialBytes {
		return errors.New("runner: child credential ceiling exceeds custody bound")
	}
	// Round-trip to own the slice independently of caller mutations.
	var owned childCredentialRecord
	if err := json.Unmarshal(data, &owned); err != nil {
		return err
	}
	in.ChildCredentials = &owned.Ceiling
	inputs[childCredentialInput] = data
	integrity[childCredentialInput] = apiv1.IntegrityTrusted
	return nil
}

// PinnedChildCredentials verifies canonical, bounded, source-bound host custody.
// It does not authorize execution: current policy and accepted source are also
// required before a credential broker resolves any raw secret.
func PinnedChildCredentials(reader *journal.Reader, id journal.RunIdentity) (*credentials.ChildCeiling, error) {
	if id.Child == nil {
		return nil, nil
	}
	var found *journal.InputRef
	for i := range id.Inputs {
		if id.Inputs[i].Name == childCredentialInput {
			if found != nil {
				return nil, errors.New("runner: duplicate child credential ceiling")
			}
			found = &id.Inputs[i]
		}
	}
	if found == nil || found.Integrity != apiv1.IntegrityTrusted {
		return nil, errors.New("runner: child credential ceiling custody missing")
	}
	data, err := reader.ArtifactBytesBounded(found.Ref, maxChildCredentialBytes)
	if err != nil {
		return nil, err
	}
	var record childCredentialRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(record)
	if err != nil || !bytes.Equal(data, canonical) || record.SourceDigest != id.Child.SourceDigest || record.EnvelopeDigest != id.Child.EnvelopeDigest {
		return nil, errors.New("runner: child credential ceiling differs from admitted source")
	}
	if err := record.Ceiling.Validate(); err != nil {
		return nil, err
	}
	return &record.Ceiling, nil
}

func childCredentialContext(ctx context.Context, in StartInput) (context.Context, error) {
	if in.Child == nil {
		return ctx, nil
	}
	if in.ChildCredentials == nil {
		return nil, errors.New("runner: child credential ceiling unavailable")
	}
	return credentials.WithChildCeiling(ctx, *in.ChildCredentials)
}

func restoreChildCredentials(reader *journal.Reader, id journal.RunIdentity, in *StartInput) error {
	ceiling, err := PinnedChildCredentials(reader, id)
	if err == nil {
		in.ChildCredentials = ceiling
	}
	return err
}

func (g gateHeartbeatGoober) reviewChildCredentials(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	if g.childCredentials != nil {
		var err error
		ctx, err = credentials.WithChildCeiling(ctx, *g.childCredentials)
		if err != nil {
			return apiv1.Verdict{}, err
		}
	}
	return invokeChildWriter(ctx, g.childWriter, g.journal, env, func(owned context.Context) (apiv1.Verdict, error) { return g.goober.Review(owned, env) })
}
