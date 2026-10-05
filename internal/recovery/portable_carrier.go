package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/goobers/goobers/internal/journal"
)

// MaxPortableCarrierBytes bounds independent workspace object custody.
const MaxPortableCarrierBytes = 16 << 20

// PortableCarrier is a bounded filtered tree with independent object custody.
// It contains synthetic ancestry and must never be published as a source branch.
type PortableCarrier struct {
	Snapshot PortableSnapshot `json:"snapshot"`
	Bundle   []byte           `json:"bundle"`
}

// PortableReturn binds a transported tree to its original execution contract.
type PortableReturn struct {
	Version        int              `json:"version"`
	ContractDigest string           `json:"contractDigest"`
	Workspace      *PortableCarrier `json:"workspace,omitempty"`
}

// Validate verifies metadata and exact archive bytes before any Git import.
func (c PortableCarrier) Validate() error {
	if err := c.Snapshot.Validate(); err != nil {
		return err
	}
	if len(c.Bundle) == 0 || len(c.Bundle) > MaxPortableCarrierBytes || int64(len(c.Bundle)) != c.Snapshot.Record.ArchiveBytes || journal.Digest(c.Bundle) != c.Snapshot.Record.ArchiveDigest {
		return fmt.Errorf("invalid isolated child workspace carrier")
	}
	return nil
}

// ReadPortableReturn verifies an already host-retained contribution artifact.
// The caller supplies the trusted artifact and contract digests, never values
// asserted by a stage. The transport's canonical wire format is unchanged.
func ReadPortableReturn(data []byte, digest, contract string) (PortableReturn, error) {
	var out PortableReturn
	if len(data) == 0 || len(data) > 24<<20 || journal.Digest(data) != digest {
		return out, fmt.Errorf("invalid portable return digest or size")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil {
		return out, err
	}
	canonical, err := json.Marshal(out)
	if err != nil || !bytes.Equal(data, canonical) || out.Version != 1 || out.ContractDigest != contract || out.Workspace == nil {
		return out, fmt.Errorf("invalid retained workspace return")
	}
	return out, out.Workspace.Validate()
}

// ImportPortableCarrier verifies and pins the archived tree without moving a
// branch, index, or working files. Callers hold managed-mirror ownership.
func ImportPortableCarrier(ctx context.Context, repository string, carrier PortableCarrier) error {
	if err := carrier.Validate(); err != nil {
		return err
	}
	f, err := os.CreateTemp("", "goobers-parent-fork-*.bundle")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if _, err = f.Write(carrier.Bundle); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return ImportPortableSnapshot(ctx, repository, f.Name(), carrier.Snapshot, MaxPortableCarrierBytes)
}
