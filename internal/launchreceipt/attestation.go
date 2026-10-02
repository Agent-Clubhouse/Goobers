package launchreceipt

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/goobers/goobers/internal/journal"
)

// AttestationSchema versions the internal stage-attempt evidence contract.
const AttestationSchema = "goobers.dev/stage-attempt-attestation/v1"

// Reference is a durable attempt-scoped locator, never proof of authority.
// Revalidate it through Reader before assigning fidelity to retrieved evidence.
// Its digest and size match the existing journal artifact addressing scheme.
type Reference struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type preparation struct {
	Source        StoreKind    `json:"source"`
	Fidelity      string       `json:"fidelity"`
	Availability  string       `json:"availability"`
	ReceiptDigest string       `json:"receiptDigest,omitempty"`
	Remote        *RemoteFacts `json:"remote,omitempty"`
	Local         *LocalFacts  `json:"local,omitempty"`
}

type actuals struct {
	Execution          string `json:"execution"`
	Completion         string `json:"completion"`
	ResolvedModel      string `json:"resolvedModel"`
	ResolvedEffort     string `json:"resolvedEffort"`
	DirectoryGrants    string `json:"directoryGrants"`
	NetworkEnforcement string `json:"networkEnforcement"`
	SandboxEnforcement string `json:"sandboxEnforcement"`
}

type attestation struct {
	Schema      string      `json:"schema"`
	Binding     Binding     `json:"binding"`
	Preparation preparation `json:"preparation"`
	Observed    actuals     `json:"observed"`
}

// Projection holds immutable canonical bytes produced by trusted assembly.
// It can populate JournalArtifactOp.Data without an additional workflow op.
// After history/storage serialization those bytes and refs are only locators;
// public readers must revalidate against the selected runtime store. No generic
// JSON decoder can construct an authenticated Projection.
type Projection struct {
	binding Binding
	raw     []byte
	ref     Reference
}

// Bytes returns a copy suitable for durable journal/history storage.
func (p Projection) Bytes() []byte { return append([]byte(nil), p.raw...) }

// Reference returns the stable attempt name and content address.
func (p Projection) Reference() Reference { return p.ref }

// Binding returns the durable controller start identity checked by assembly.
func (p Projection) Binding() Binding { return p.binding }

// Assemble reads a single bounded receipt and constructs a bounded v1 record.
// limit applies to both input and output (at most MaxBytes). Missing and legacy
// evidence explicitly remain unknown. Run-finished or inferred stage completion
// is never considered: this contract authenticates only prepared configuration.
func (r Reader) Assemble(ctx context.Context, expected Binding, limit int) (Projection, error) {
	evidence, err := r.read(ctx, expected, limit)
	if err != nil {
		return Projection{}, err
	}
	record := attestation{Schema: AttestationSchema, Binding: expected,
		Preparation: evidence.preparation(), Observed: actuals{
			Execution: "unknown", Completion: "unknown", ResolvedModel: "unknown",
			ResolvedEffort: "unknown", DirectoryGrants: "unknown",
			NetworkEnforcement: "unknown", SandboxEnforcement: "unknown",
		},
	}
	raw, err := json.Marshal(record)
	if err != nil || len(raw) > limit {
		return Projection{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Projection{}, err
	}
	return Projection{binding: expected, raw: raw, ref: Reference{
		Name:   "stage-attempt-attestation-" + expected.AttemptID + ".json",
		Digest: journal.Digest(raw), Size: int64(len(raw)),
	}}, nil
}

func (e evidence) preparation() preparation {
	p := preparation{Source: e.store, Fidelity: "unknown", Availability: "missing"}
	if e.receipt == nil {
		return p
	}
	p.Availability = "available"
	p.Fidelity = "unverified"
	p.ReceiptDigest = "sha256:" + e.digest
	if e.store == ControlPlaneStore {
		p.Fidelity = "authenticated-control-plane-preparation"
	}
	if e.receipt.Local != nil {
		p.Local = e.receipt.Local
	} else {
		p.Remote = &e.receipt.Facts
	}
	return p
}

// Revalidate treats every persisted reference as an untrusted locator. It
// re-reads the selected trusted source, checks the entire expected binding, and
// rebuilds canonical bytes before comparing name, digest, and size. A forged
// generic artifact can never establish fidelity through this method.
func (r Reader) Revalidate(ctx context.Context, expected Binding, ref Reference, limit int) (Projection, error) {
	if !expected.valid() || ref.Name != "stage-attempt-attestation-"+expected.AttemptID+".json" ||
		(!strings.HasPrefix(ref.Digest, "sha256:") || !ValidDigest(ref.Digest)) || ref.Size < 1 || ref.Size > MaxBytes {
		return Projection{}, ErrInvalid
	}
	projection, err := r.Assemble(ctx, expected, limit)
	if err != nil {
		return Projection{}, err
	}
	if projection.ref != ref {
		return Projection{}, ErrInvalid
	}
	return projection, nil
}
