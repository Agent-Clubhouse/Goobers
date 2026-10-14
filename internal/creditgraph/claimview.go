package creditgraph

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/goobers/goobers/internal/journal"
)

// The run's claim log is readable by later stages (#7119). Before an agentic
// stage starts, the runner snapshots the claims journaled earlier in the run
// into RunClaimsFileName in the stage's GOOBERS_TELEMETRY_DIR, and `goobers
// telemetry claims` prints it. The snapshot is a read view of the journal: it
// lists recorded claims with their runner-attributed writer, re-validated and
// bounded exactly as Build reads them, but it does not verify them — that
// happens when Backprop builds the run's credit graph.
const (
	// RunClaimsFileName is the runner-written snapshot of earlier claims.
	RunClaimsFileName = "run-claims.json"
	// RunClaimsSchemaVersion identifies the snapshot's wire shape.
	RunClaimsSchemaVersion = "goobers.dev/backprop/run-claims/v1"

	maxRunClaimsBytes = MaxClaimsPerRun*maxClaimLineBytes + 64<<10
)

// RunClaim is one claim recorded earlier in the run. ID matches the
// ClaimRecord ID Backprop later reports for the same claim.
type RunClaim struct {
	ID     string      `json:"id"`
	Seq    uint64      `json:"seq"`
	Writer ClaimWriter `json:"writer"`
	Claim
}

// RunClaims is the snapshot of a run's claim log as of one stage's start.
// Error is set when the runner could not read the journal, so a reader never
// mistakes an unavailable log for an empty one.
type RunClaims struct {
	Schema  string     `json:"schema"`
	Claims  []RunClaim `json:"claims"`
	Dropped int        `json:"dropped,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// CollectRunClaims returns the claims journaled in events, in journal order,
// with the same per-run bound and re-validation Build applies.
func CollectRunClaims(events []journal.Event) RunClaims {
	view := RunClaims{Schema: RunClaimsSchemaVersion, Claims: []RunClaim{}}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner == nil {
			continue
		}
		if kind, _ := event.Runner[SpanProvenanceKeyKind].(string); kind != ClaimsAnnotation {
			continue
		}
		claims, writerAgent, dropped := decodeClaimsAnnotation(event.Runner)
		view.Dropped += dropped
		writer := ClaimWriter{Stage: event.Stage, Attempt: max(event.Attempt, 1), Agent: writerAgent}
		for index, claim := range claims {
			if len(view.Claims) >= MaxClaimsPerRun {
				view.Dropped++
				continue
			}
			view.Claims = append(view.Claims, RunClaim{
				ID:     "claim:" + strconv.FormatUint(event.Seq, 10) + ":" + strconv.Itoa(index),
				Seq:    event.Seq,
				Writer: writer,
				Claim:  claim,
			})
		}
	}
	return view
}

// WriteRunClaims replaces dir's snapshot with view. It never follows a
// symlink planted at the snapshot path.
func WriteRunClaims(dir string, view RunClaims) error {
	data, err := json.Marshal(view)
	if err != nil {
		return fmt.Errorf("encode run claims: %w", err)
	}
	path := filepath.Join(dir, RunClaimsFileName)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace run claims: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create run claims: %w", err)
	}
	_, writeErr := file.Write(append(data, '\n'))
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return fmt.Errorf("write run claims: %w", writeErr)
	}
	return nil
}

// ReadRunClaims reads dir's snapshot. The file sits in an agent-writable
// directory, so it is bounded, and every entry is re-validated: an entry that
// fails the claim schema is counted as dropped rather than returned.
func ReadRunClaims(dir string) (RunClaims, error) {
	if dir == "" {
		return RunClaims{}, errors.New("no stage telemetry directory (GOOBERS_TELEMETRY_DIR is unset)")
	}
	path := filepath.Join(dir, RunClaimsFileName)
	info, err := os.Lstat(path)
	if err != nil {
		return RunClaims{}, fmt.Errorf("no run claim snapshot for this stage: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxRunClaimsBytes {
		return RunClaims{}, fmt.Errorf("%s is not a bounded regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return RunClaims{}, fmt.Errorf("open run claims: %w", err)
	}
	defer func() { _ = file.Close() }()
	var raw struct {
		Schema  string            `json:"schema"`
		Claims  []json.RawMessage `json:"claims"`
		Dropped int               `json:"dropped"`
		Error   string            `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(file, maxRunClaimsBytes)).Decode(&raw); err != nil {
		return RunClaims{}, fmt.Errorf("decode run claims: %w", err)
	}
	if raw.Schema != RunClaimsSchemaVersion {
		return RunClaims{}, fmt.Errorf("run claims schema %q is not %q", raw.Schema, RunClaimsSchemaVersion)
	}
	if raw.Error != "" {
		return RunClaims{}, fmt.Errorf("the runner could not read this run's claim log: %s", SanitizeClaimText(raw.Error))
	}
	view := RunClaims{Schema: raw.Schema, Claims: []RunClaim{}, Dropped: max(0, min(raw.Dropped, maxRunClaimsBytes))}
	for _, entry := range raw.Claims {
		claim, ok := decodeRunClaim(entry)
		if !ok || len(view.Claims) >= MaxClaimsPerRun {
			view.Dropped++
			continue
		}
		view.Claims = append(view.Claims, claim)
	}
	return view, nil
}

func decodeRunClaim(data []byte) (RunClaim, bool) {
	var envelope struct {
		ID     string      `json:"id"`
		Seq    uint64      `json:"seq"`
		Writer ClaimWriter `json:"writer"`
	}
	if json.Unmarshal(data, &envelope) != nil || envelope.ID == "" || !validClaimRef(envelope.ID) ||
		envelope.Writer.Stage == "" || !validClaimRef(envelope.Writer.Stage) || !validClaimRef(envelope.Writer.Agent) ||
		envelope.Writer.Attempt < 1 {
		return RunClaim{}, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return RunClaim{}, false
	}
	for _, key := range []string{"id", "seq", "writer"} {
		delete(fields, key)
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return RunClaim{}, false
	}
	claim, ok := decodeClaim(body)
	if !ok {
		return RunClaim{}, false
	}
	return RunClaim{ID: envelope.ID, Seq: envelope.Seq, Writer: envelope.Writer, Claim: claim}, true
}
