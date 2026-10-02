package launchreceipt

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/durability"
)

// Verifier authenticates launch authority and enforces its expiry.
type Verifier interface{ VerifyLaunchGrant(string) (Grant, error) }

// Store must live in daemon-owned state, never a stage-mounted run directory.
// The receipt itself is the durable consumption marker. Any incomplete write
// remains consumed: a crash cannot permit a second launch under the same ID.
type Store struct {
	root     string
	verifier Verifier
}

// NewStore opens a daemon-private receipt directory.
func NewStore(root string, verifier Verifier) (*Store, error) {
	if root == "" || verifier == nil {
		return nil, ErrInvalid
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	if err := durability.SyncDir(filepath.Dir(root)); err != nil {
		return nil, err
	}
	return &Store{root: root, verifier: verifier}, nil
}

// Accept consumes authority once and synchronously persists its bound receipt.
func (s *Store) Accept(ctx context.Context, token string, r Receipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := r.Encode()
	if err != nil {
		return err
	}
	g, err := s.verifier.VerifyLaunchGrant(token)
	if err != nil || g.AttemptID != r.Binding.AttemptID || g.Digest != Digest(raw) {
		return ErrInvalid
	}
	f, err := os.OpenFile(filepath.Join(s.root, g.AttemptID+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return ErrUsed
	}
	if err != nil {
		return err
	}
	_, writeErr := f.Write(raw)
	syncErr := f.Sync()
	closeErr := f.Close()
	dirErr := durability.SyncDir(s.root)
	return errors.Join(writeErr, syncErr, closeErr, dirErr, ctx.Err())
}
