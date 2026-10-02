// Package mutationreceipt defines inert semantic provider-mutation capture and
// evidence contracts. Reconciliation requires fresh provider-specific evidence.
package mutationreceipt

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Identity identifies a single provider action before attribution is
// added. Repository must be the provider's canonical API repository identity,
// including its host and (for ADO) organization/project. Target distinguishes
// issues, pull requests, comments, and individual labels within that repository.
// ContentDigest hashes only semantic input: never run IDs, costs, or footers.
// This metadata is not authority to skip or retry any provider operation.
type Identity struct {
	Provider      string `json:"provider"`
	Repository    string `json:"repository"`
	Action        string `json:"action"`
	Target        string `json:"target"`
	ContentDigest string `json:"contentDigest"`
}

// Receipt is versioned evidence for one invocation. An intent
// alone means unknown outcome, including a crash before dispatch or after the
// provider commits. Completion means the provider callback confirmed success;
// it does not establish that future provider state still matches this action.
// ID is opaque and shared by intent/completion; sidecar custody IDs are separate.
type Receipt struct {
	Schema   int      `json:"schema"`
	ID       string   `json:"id"`
	RunID    string   `json:"runId"`
	Mutation Identity `json:"mutation"`
	Phase    string   `json:"phase"`
}

// Recorder durably stores an intent before dispatch and a
// completion after provider success. Implementations must return only after
// flushing the record and its containing directory or receiving a durable ack.
// Wiring this contract into provider paths is deliberately deferred to #6356's
// reconcile and activation changes; existing MutationRecorder is unchanged.
type Recorder interface {
	RecordSemanticMutation(context.Context, Receipt) error
}

// New hashes JSON semantic content using Go's deterministic
// map-key encoding. Callers supply typed values (not raw JSON), normalize sets
// and provider-specific casing, and exclude attribution before calling. No
// plaintext payload is retained. Different fields or repository hosts differ.
func New(provider, repository, action, target string, content any) (Identity, error) {
	encoded, err := json.Marshal(content)
	if err != nil {
		return Identity{}, fmt.Errorf("encode semantic mutation content: %w", err)
	}
	// Re-encode objects in key order even when callers use different struct
	// field order. UseNumber preserves large integers without float rounding.
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var normalized any
	if err := decoder.Decode(&normalized); err != nil {
		return Identity{}, fmt.Errorf("normalize semantic mutation content: %w", err)
	}
	encoded, err = json.Marshal(normalized)
	if err != nil {
		return Identity{}, fmt.Errorf("normalize semantic mutation content: %w", err)
	}
	sum := sha256.Sum256(encoded)
	mutation := Identity{Provider: provider, Repository: repository, Action: action, Target: target, ContentDigest: "sha256:" + hex.EncodeToString(sum[:])}
	if err := mutation.validate(); err != nil {
		return Identity{}, err
	}
	return mutation, nil
}

func (m Identity) validate() error {
	for _, value := range []string{m.Provider, m.Action, m.Target} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("semantic mutation requires provider, repository, action, and target")
		}
	}
	repo, err := url.Parse(m.Repository)
	if err != nil || repo.Host == "" || (repo.Scheme != "https" && repo.Scheme != "http") || repo.User != nil || repo.RawQuery != "" || repo.Fragment != "" {
		return fmt.Errorf("semantic mutation repository must be a credential-free API repository URL")
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(m.ContentDigest, "sha256:"))
	if !strings.HasPrefix(m.ContentDigest, "sha256:") || err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != strings.TrimPrefix(m.ContentDigest, "sha256:") {
		return fmt.Errorf("semantic mutation content digest is invalid")
	}
	return nil
}

// Validate rejects unknown versions/phases and incomplete identities rather
// than allowing an unsupported receipt to become continuation evidence.
func (r Receipt) Validate() error {
	if r.Schema != 1 || r.ID == "" || r.RunID == "" || (r.Phase != "intent" && r.Phase != "completed") {
		return fmt.Errorf("invalid semantic mutation receipt identity, version, or phase")
	}
	return r.Mutation.validate()
}

// Capture is an opt-in capture boundary, not a replay policy.
// Every call invokes action once after its intent is durable, even when another
// invocation has identical content. Errors and panics leave the intent unknown.
// A completion-write error is returned without retrying the already completed
// action. The callback receives the intent identity for a future provider-side
// reconciliation marker; Capture does not alter provider payloads.
// The recorder must not share the run journal's writer across processes.
func Capture(ctx context.Context, recorder Recorder, runID string, mutation Identity, action func(context.Context, Receipt) error) error {
	if recorder == nil || action == nil {
		return fmt.Errorf("semantic mutation capture requires recorder and action")
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return fmt.Errorf("create semantic mutation identity: %w", err)
	}
	receipt := Receipt{Schema: 1, ID: hex.EncodeToString(random[:]), RunID: runID, Mutation: mutation, Phase: "intent"}
	if err := receipt.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := recorder.RecordSemanticMutation(ctx, receipt); err != nil {
		return fmt.Errorf("persist semantic mutation intent: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := action(ctx, receipt); err != nil {
		return err
	}
	receipt.Phase = "completed"
	if err := recorder.RecordSemanticMutation(context.WithoutCancel(ctx), receipt); err != nil {
		return fmt.Errorf("persist semantic mutation completion: %w", err)
	}
	return nil
}
