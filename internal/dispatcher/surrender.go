package dispatcher

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/platform/durability"
)

// surrender.go is the surrendered-result half of the disposal gate
// (architecture §3: "its ResultEnvelope into the engine"): the plane through
// which a stage pod's business outcome travels back to the engine's dispatch
// activity.
//
// This is deliberately NOT blobstore.Store. The artifact store is
// content-addressed and its readers VERIFY the digest against the bytes
// (blobstore.Dir.Get fails a mismatch closed), but the engine must find "the
// result of run R, stage S, attempt N" BEFORE knowing its content — an
// identity-keyed lookup the content-addressed contract structurally cannot
// serve. So surrendered results ride their own narrow plane, keyed by attempt
// identity, write-once per key by construction: one attempt is one pod (D1),
// and a retried attempt carries a fresh Number and therefore a fresh key.
//
// The pod-side WRITER is the in-pod stage runtime's (the image contract,
// goobernetes-deployment-images.md): before exiting, it marshals a
// SurrenderedResult and Puts it under its attempt identity — LAST, after
// artifact/span write-through and journal emits, so presence of the result
// implies the rest of the surrender contract held. The engine-side READER is
// engine.(*Activities).DispatchStage (#3588), which turns the document back
// into the stageActivityResult the workflow consumes with parity to the
// in-process InvokeGoober / RunDeterministic activities.

// ErrNoSurrender is returned by SurrenderPlane.Get when no result was
// surrendered for the attempt. Distinguished so the caller can classify
// "surrender confirmed but result absent" precisely.
var ErrNoSurrender = errors.New("dispatcher: no surrendered result for attempt")

// SurrenderPlane stores and serves surrendered result documents by attempt
// identity.
type SurrenderPlane interface {
	// Get returns the surrendered result document, or an error wrapping
	// ErrNoSurrender.
	Get(ctx context.Context, runID, stage string, attempt int) ([]byte, error)
	// Has reports presence — the disposal gate's question.
	Has(ctx context.Context, runID, stage string, attempt int) (bool, error)
	// Put stores the document. Storing an already-present attempt succeeds
	// without rewriting (idempotent, like the blob plane's Put).
	Put(ctx context.Context, runID, stage string, attempt int, data []byte) error
	// Describe names the plane for diagnostics, credential-free.
	Describe() string
}

// SurrenderedMutation is one provider mutation fact a stage pod observed,
// mirroring the engine's mutation sidecar shape field for field so the
// projected journal records identical provenance whichever substrate ran the
// stage.
type SurrenderedMutation struct {
	Provider  string `json:"provider"`
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	URL       string `json:"url,omitempty"`
	Operation string `json:"operation,omitempty"`
}

// SurrenderedResult is the wire shape of one attempt's surrendered outcome:
// exactly what the engine's in-process activities produce for a local stage
// (ResultEnvelope + mutation facts + mutation issues), so the dispatch
// activity can marshal it back into the identical stageActivityResult.
type SurrenderedResult struct {
	// Result is the stage's ResultEnvelope — the business outcome. A pod
	// whose stage failed surrenders a ResultFailure envelope, the same honest
	// status the local executor returns, never a bare process exit.
	Result apiv1.ResultEnvelope `json:"result"`
	// Mutations are the provider mutation facts from the stage's mutation
	// sidecar (mutations.jsonl parity).
	Mutations []SurrenderedMutation `json:"mutations,omitempty"`
	// MutationIssues carry malformed-provenance notes without converting a
	// successful stage into a failure (the sidecar's own rule).
	MutationIssues []string `json:"mutationIssues,omitempty"`
	// WorkspaceDelta is the blob digest of a git bundle carrying whatever this
	// stage committed beyond the run's base (#3763). Empty when the stage has
	// no repo workspace or committed nothing, which is the common case.
	//
	// The engine threads this to the NEXT stage exactly as it already threads
	// workspaceBranch: a pod is disposed after surrender, so a commit that does
	// not leave through here does not exist for anything downstream.
	WorkspaceDelta string `json:"workspaceDelta,omitempty"`
	// WorkspaceDeltaBase and WorkspaceDeltaTip are the two commits the bundle
	// was cut between (base..tip), surrendered beside the digest so the
	// engine can journal them (runner.workspace.delta) and a far-side reader
	// can compare the next stage's checkout against the tip by SHA rather
	// than by trusting the digest. Both empty whenever WorkspaceDelta is.
	WorkspaceDeltaBase string `json:"workspaceDeltaBase,omitempty"`
	WorkspaceDeltaTip  string `json:"workspaceDeltaTip,omitempty"`
	// WorkspaceDeltaUnchanged reports that this stage ran on a WRITABLE repo
	// workspace, succeeded, and the pod CHECKED that its branch carries no
	// commits beyond base — so no bundle was published. It is a positive
	// claim the pod makes, distinct from an absent WorkspaceDelta: a stage on
	// a scratch or read-only workspace, a failed stage, or a stage image that
	// predates this field all surrender neither, and the engine journals
	// nothing about the branch rather than inferring "unchanged" from
	// silence. Never set beside a non-empty WorkspaceDelta.
	WorkspaceDeltaUnchanged bool `json:"workspaceDeltaUnchanged,omitempty"`
	// Verdict is the reviewer's decision when the attempt was an agentic
	// reviewer GATE evaluated in a pod (Attempt.Review, agentickit.ModeReview;
	// decision 001 rulings 7–8) — the same apiv1.Verdict the worker's
	// ReviewGoober activity returns in-process. Result then carries a bare
	// success status (the harness session completed), never a business
	// outcome: the verdict IS the outcome, and the engine routes on
	// Verdict.Decision alone. Nil for every task attempt.
	//
	// The engine RE-VALIDATES what comes back (fail closed on an empty
	// Decision and on the verdict schema) rather than trusting the pod's own
	// harness validation: a substituted or truncated surrender document must
	// never route control flow (#3838's shape).
	Verdict *apiv1.Verdict `json:"verdict,omitempty"`
}

// ReadSurrenderedResult fetches and decodes one attempt's surrendered result.
func ReadSurrenderedResult(ctx context.Context, plane SurrenderPlane, runID, stage string, attempt int) (SurrenderedResult, error) {
	if plane == nil {
		return SurrenderedResult{}, fmt.Errorf("dispatcher: no surrender plane configured for run %s stage %s attempt %d", runID, stage, attempt)
	}
	data, err := plane.Get(ctx, runID, stage, attempt)
	if err != nil {
		return SurrenderedResult{}, fmt.Errorf("dispatcher: fetch surrendered result for run %s stage %s attempt %d: %w", runID, stage, attempt, err)
	}
	var result SurrenderedResult
	if err := json.Unmarshal(data, &result); err != nil {
		return SurrenderedResult{}, fmt.Errorf("dispatcher: decode surrendered result for run %s stage %s attempt %d: %w", runID, stage, attempt, err)
	}
	return result, nil
}

// PlaneSurrenderGate is a SurrenderGate answering from the surrender plane:
// an attempt's outputs are surrendered exactly when its result document is
// present (the pod writes it last, after everything else surrendered).
type PlaneSurrenderGate struct {
	Plane SurrenderPlane
}

// Confirmed reports whether the attempt's surrendered result exists.
func (g PlaneSurrenderGate) Confirmed(ctx context.Context, attempt Attempt) (bool, error) {
	if g.Plane == nil {
		return false, errors.New("dispatcher: surrender gate has no plane")
	}
	return g.Plane.Has(ctx, attempt.RunID, attempt.Stage, attempt.Number)
}

// SurrenderDir is a SurrenderPlane backed by a directory — the same stance as
// blobstore.Dir: whether the directory is a shared volume or a mount over
// object storage is the operator's business. Each attempt's document lives in
// one file named by the hash of its identity triple, so no run or stage
// spelling can traverse outside Root.
type SurrenderDir struct {
	Root string
}

// NewSurrenderDir returns a directory-backed plane rooted at root, creating
// it if needed.
func NewSurrenderDir(root string) (*SurrenderDir, error) {
	if root == "" {
		return nil, errors.New("dispatcher: surrender plane needs a root directory")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("dispatcher: create surrender plane root %q: %w", root, err)
	}
	return &SurrenderDir{Root: root}, nil
}

// Describe reports the plane's root path.
func (d *SurrenderDir) Describe() string { return "surrender-dir:" + d.Root }

// surrenderFileVersion versions the on-disk naming; an incompatible change to
// SurrenderedResult must change it so an old pod's document is never decoded
// as the new shape.
const surrenderFileVersion = "goobers.dev/surrender/v1"

func (d *SurrenderDir) path(runID, stage string, attempt int) (string, error) {
	if d == nil || d.Root == "" {
		return "", errors.New("dispatcher: surrender plane has no root")
	}
	if runID == "" || stage == "" || attempt < 1 {
		return "", fmt.Errorf("dispatcher: surrender identity requires run, stage, and a positive attempt (got run %q stage %q attempt %d)", runID, stage, attempt)
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s\x00%d", surrenderFileVersion, runID, stage, attempt))
	return filepath.Join(d.Root, fmt.Sprintf("%x.json", sum)), nil
}

// Get reads one attempt's document.
func (d *SurrenderDir) Get(ctx context.Context, runID, stage string, attempt int) ([]byte, error) {
	path, err := d.path(runID, stage, attempt)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: run %s stage %s attempt %d", ErrNoSurrender, runID, stage, attempt)
		}
		return nil, fmt.Errorf("dispatcher: read surrendered result: %w", err)
	}
	return data, nil
}

// Has reports whether the attempt's document exists.
func (d *SurrenderDir) Has(ctx context.Context, runID, stage string, attempt int) (bool, error) {
	path, err := d.path(runID, stage, attempt)
	if err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	switch _, err := os.Stat(path); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("dispatcher: stat surrendered result: %w", err)
	}
}

// Put writes one attempt's document, staged-and-renamed so a concurrent
// reader never observes a half-written result. Re-putting an existing attempt
// succeeds without rewriting: the key is write-once by construction (one
// attempt, one pod — D1), so a duplicate Put can only be the same pod
// retrying its own surrender.
func (d *SurrenderDir) Put(ctx context.Context, runID, stage string, attempt int, data []byte) error {
	path, err := d.path(runID, stage, attempt)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("dispatcher: stat surrendered result: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("dispatcher: create surrender dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".surrender-*")
	if err != nil {
		return fmt.Errorf("dispatcher: stage surrendered result: %w", err)
	}
	staged := tmp.Name()
	defer func() { _ = os.Remove(staged) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("dispatcher: write surrendered result: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("dispatcher: sync surrendered result: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("dispatcher: close surrendered result: %w", err)
	}
	if err := durability.ReplaceFile(staged, path); err != nil {
		if _, statErr := os.Stat(path); statErr == nil {
			// The same pod's retry already landed the identical document.
			return nil
		}
		return fmt.Errorf("dispatcher: publish surrendered result: %w", err)
	}
	if err := durability.SyncDir(dir); err != nil {
		return fmt.Errorf("dispatcher: sync surrender dir: %w", err)
	}
	return nil
}
