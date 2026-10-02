package launchreceipt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/goobers/goobers/internal/platform/safeopen"
)

// RuntimeStore identifies authority selected by trusted runtime configuration.
// Root must never come from an offline --path, a journal artifact, or a receipt.
// A protected control-plane store authenticates preparation only. Local stores
// are always unverified, including ones containing remote-shaped receipts.
type RuntimeStore struct {
	Root string
	Kind StoreKind
}

// StoreKind is chosen by the runtime resolver, never inferred from a filename
// or a receipt's self-reported source.
type StoreKind string

const (
	// ControlPlaneStore is a protected daemon-owned receipt store.
	ControlPlaneStore StoreKind = "control-plane"
	// LocalStore cannot establish integrity against same-UID execution.
	LocalStore StoreKind = "local-runtime"
	// UnknownStore describes a legacy attempt with no known receipt authority.
	UnknownStore StoreKind = "unknown"
)

// RuntimeResolver is a trusted composition boundary. Resolve exactly one store
// from the run's runtime configuration; do not probe alternative stores after
// a miss. Implementations must honor ctx and must not trust model/journal data
// to choose a root or elevate a local run to control-plane authority.
type RuntimeResolver interface {
	ResolveReceiptStore(context.Context, Binding) (RuntimeStore, error)
}

// Reader assembles evidence only through a trusted runtime resolver. It is an
// internal primitive; arbitrary artifact bytes cannot be supplied as evidence.
type Reader struct{ Runtime RuntimeResolver }

var errRead = errors.New("launch receipt unavailable")

// evidence is sealed: only the bounded trusted-store reader can construct it.
// An empty receipt means missing evidence, never successful execution.
type evidence struct {
	store   StoreKind
	receipt *Receipt
	digest  string
}

func (r Reader) read(ctx context.Context, expected Binding, limit int) (evidence, error) {
	if err := ctx.Err(); err != nil {
		return evidence{}, err
	}
	// Validate every identity field before resolving a root or deriving a path.
	if !expected.valid() || limit < 1 || limit > MaxBytes || r.Runtime == nil {
		return evidence{}, ErrInvalid
	}
	location, err := r.Runtime.ResolveReceiptStore(ctx, expected)
	if err != nil {
		return evidence{}, readError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return evidence{}, err
	}
	if location.Kind == UnknownStore && location.Root == "" {
		return evidence{store: UnknownStore}, nil
	}
	if location.Root == "" || (location.Kind != ControlPlaneStore && location.Kind != LocalStore) {
		return evidence{}, ErrInvalid
	}
	raw, err := readBody(ctx, location.Root, expected.AttemptID+".json", limit)
	if errors.Is(err, os.ErrNotExist) {
		return evidence{store: location.Kind}, nil
	}
	if err != nil {
		return evidence{}, err
	}
	receipt, err := decodeReceipt(raw, expected)
	if err != nil {
		return evidence{}, err
	}
	if location.Kind == ControlPlaneStore && receipt.Local != nil {
		return evidence{}, ErrInvalid
	}
	return evidence{store: location.Kind, receipt: &receipt, digest: Digest(raw)}, nil
}

func readBody(ctx context.Context, rootPath, name string, limit int) ([]byte, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, readError(ctx, err)
	}
	defer func() { _ = root.Close() }()
	file, err := safeopen.OpenRegularInRoot(root, name)
	if err != nil {
		return nil, readError(ctx, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, readError(ctx, err)
	}
	if info.Size() < 1 || info.Size() > int64(limit) {
		return nil, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Stat is insufficient: the file could grow before/during the read.
	raw, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, readError(ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(raw) > limit || len(raw) != int(info.Size()) {
		return nil, ErrInvalid
	}
	return raw, nil
}

func readError(ctx context.Context, err error) error {
	if canceled := ctx.Err(); canceled != nil {
		return canceled
	}
	if errors.Is(err, os.ErrNotExist) {
		return os.ErrNotExist
	}
	// Never expose filesystem paths, resolver errors, or credentials to callers.
	return errRead
}

func decodeReceipt(raw []byte, expected Binding) (Receipt, error) {
	var receipt Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt.Binding != expected {
		return Receipt{}, ErrInvalid
	}
	canonical, err := receipt.Encode()
	// Byte equality rejects unknown/duplicate keys, whitespace, trailing values,
	// alternate number spellings, and every other noncanonical representation.
	if err != nil || !bytes.Equal(raw, canonical) {
		return Receipt{}, ErrInvalid
	}
	return receipt, nil
}
