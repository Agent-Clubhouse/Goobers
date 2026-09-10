package providers

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/sharedclaim"
)

const sharedRefPrefix = "refs/heads/goobers-shared-claims/"

// ReconcileSharedVisibility discovers durable records independently of the
// local ledger. cursor is the last attempted ref, not a provider URL. A caller
// repeats bounded passes, including after errors, and starts a new sweep when
// the returned cursor is empty. Failed items are retried on the next sweep;
// one malformed record or unavailable issue cannot starve later records.
func (s GitHubSharedClaimStore) ReconcileSharedVisibility(ctx context.Context, labels sharedclaim.Visibility, cursor string, limit int) (string, error) {
	if labels == nil || limit < 1 || limit > 128 || (cursor != "" && !validSharedInventoryRef(cursor)) {
		return cursor, fmt.Errorf("invalid shared visibility sweep")
	}
	refs, err := s.sharedVisibilityRefs(ctx)
	if err != nil {
		return cursor, err
	}
	var failures error
	attempted := 0
	for _, ref := range refs {
		if ref.Ref <= cursor {
			continue
		}
		if attempted == limit {
			return cursor, failures
		}
		if err := ctx.Err(); err != nil {
			return cursor, errors.Join(failures, err)
		}
		attempted++
		cursor = ref.Ref
		if err := s.reconcileSharedRef(ctx, labels, ref); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return "", failures
}

func (s GitHubSharedClaimStore) sharedVisibilityRefs(ctx context.Context) ([]sharedGitRef, error) {
	endpoint, err := s.endpoint("matching-refs", strings.TrimPrefix(sharedRefPrefix, "refs/"))
	if err != nil {
		return nil, err
	}
	// joinURL trims trailing slashes; restore this namespace boundary so a
	// similarly named ordinary branch is not included in prefix matching.
	response, err := s.Provider.send(ctx, http.MethodGet, endpoint+"/", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("shared visibility inventory refused (HTTP %d)", response.StatusCode)
	}
	// Matching refs has no pagination. Refuse oversized inventories explicitly
	// rather than mistaking a truncated list for a completed sweep.
	const maxBytes = 4 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBytes {
		return nil, fmt.Errorf("shared visibility inventory exceeds bound")
	}
	if !strings.HasPrefix(strings.TrimSpace(string(data)), "[") {
		return nil, fmt.Errorf("shared visibility inventory must be an array")
	}
	var refs []sharedGitRef
	if err := json.Unmarshal(data, &refs); err != nil {
		return nil, err
	}
	for _, ref := range refs {
		if !validSharedInventoryRef(ref.Ref) {
			return nil, fmt.Errorf("shared visibility inventory contains a foreign reference")
		}
	}
	slices.SortFunc(refs, func(a, b sharedGitRef) int { return strings.Compare(a.Ref, b.Ref) })
	return slices.CompactFunc(refs, func(a, b sharedGitRef) bool { return a.Ref == b.Ref }), nil
}

func validSharedInventoryRef(ref string) bool {
	digest, ok := strings.CutPrefix(ref, sharedRefPrefix)
	if !ok || len(digest) != 64 || digest != strings.ToLower(digest) {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func (s GitHubSharedClaimStore) reconcileSharedRef(ctx context.Context, labels sharedclaim.Visibility, ref sharedGitRef) error {
	if ref.Object.Type != "commit" || !sharedGitSHA(ref.Object.SHA) {
		return fmt.Errorf("invalid shared visibility reference object")
	}
	commit, err := s.commit(ctx, ref.Object.SHA)
	if err != nil {
		return err
	}
	var envelope struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(commit.Message), &envelope); err != nil {
		return fmt.Errorf("invalid shared visibility envelope")
	}
	if ref.Ref != sharedClaimRef(envelope.Key) {
		return fmt.Errorf("shared visibility key does not match reference")
	}
	if _, err := sharedclaim.Decode(envelope.Key, []byte(commit.Message)); err != nil {
		return err
	}
	number, err := strconv.ParseUint(envelope.Key, 10, 64)
	if err != nil || number == 0 || strconv.FormatUint(number, 10) != envelope.Key {
		return nil // Valid auxiliary coordination records have no issue label.
	}
	// Inventory commits are discovery hints. The reconciler always re-reads
	// the latest authoritative ref before and after touching its label.
	return sharedclaim.ReconcileVisibility(ctx, s, labels, envelope.Key)
}
