package spec

import (
	"encoding/json"
	"errors"
	"sort"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/worktree"
)

// JoinKind records host preparation, application readiness and acknowledgement.
const JoinKind = "isolated.parallel.join"

// MaxJoinMetadataBytes allows the complete bounded branch set and long run IDs.
const MaxJoinMetadataBytes = 512 << 10

// JoinResult binds one stopped branch to its immutable result carrier.
type JoinResult struct {
	Branch  int                   `json:"branch"`
	Status  journal.BranchStatus  `json:"status"`
	Custody worktree.StageCustody `json:"custody"`
	Source  Source                `json:"source"`
}

// JoinRequest is chosen by the root dispatcher after all branch writers stop.
// Results includes every branch; only succeeded branches contribute code.
type JoinRequest struct {
	Request
	Plan    journal.Ref           `json:"plan"`
	Seed    Source                `json:"seed"`
	Root    worktree.StageCustody `json:"root"`
	Results []JoinResult          `json:"results"`
}

// JoinReceipt binds every transition to one exact parallel visit and intent.
type JoinReceipt struct {
	Sequence uint64      `json:"sequence"`
	Intent   journal.Ref `json:"intent"`
	Ready    journal.Ref `json:"ready,omitempty"`
}

// JoinState is a validated ordered journal projection, not workspace proof.
type JoinState struct {
	JoinReceipt
	Parallel string
	Applied  bool
}

// ReadJoins rejects reordered, substituted or branch-scoped application records.
// Cleanup uses this neutral projection to fence all unfinished applications.
func ReadJoins(reader *journal.Reader) (map[uint64]*JoinState, error) {
	events, err := reader.Events()
	if err != nil {
		return nil, err
	}
	states := map[uint64]*JoinState{}
	for _, event := range events {
		if event.Type != journal.EventRunnerAnnotation || event.Runner["kind"] != JoinKind {
			continue
		}
		if err := consumeJoin(reader, states, event); err != nil {
			return nil, err
		}
	}
	return states, nil
}

func consumeJoin(reader *journal.Reader, states map[uint64]*JoinState, event journal.Event) error {
	var value JoinReceipt
	data, err := json.Marshal(event.Runner["join"])
	if err != nil || len(data) > 4096 || json.Unmarshal(data, &value) != nil || event.Branch != 0 || value.Sequence == 0 || value.Sequence >= event.Seq || event.Parallel == "" {
		return errors.New("invalid parallel join receipt")
	}
	if err := readJoinRef(reader, value.Intent); err != nil {
		return err
	}
	state := states[value.Sequence]
	if event.Runner["phase"] == "prepared" {
		if state != nil || len(states) >= 128 || value.Ready != (journal.Ref{}) {
			return errors.New("duplicate or oversized parallel join preparation")
		}
		states[value.Sequence] = &JoinState{JoinReceipt: value, Parallel: event.Parallel}
		return nil
	}
	if state == nil || state.Applied || state.Intent != value.Intent || state.Parallel != event.Parallel {
		return errors.New("parallel join transition changed intent")
	}
	if err := readJoinRef(reader, value.Ready); err != nil {
		return err
	}
	return advanceJoin(state, value, event.Runner["phase"])
}

func advanceJoin(state *JoinState, value JoinReceipt, phase any) error {
	switch phase {
	case "ready":
		if state.Ready != (journal.Ref{}) {
			return errors.New("duplicate parallel join application plan")
		}
		state.Ready = value.Ready
	case "applied":
		if state.Ready != value.Ready {
			return errors.New("parallel join acknowledgement changed application plan")
		}
		state.Applied = true
	default:
		return errors.New("unknown parallel join transition")
	}
	return nil
}

func readJoinRef(reader *journal.Reader, ref journal.Ref) error {
	if ref.Integrity != apiv1.IntegrityTrusted {
		return errors.New("parallel join lacks host provenance")
	}
	_, err := reader.ArtifactBytesBounded(ref, MaxJoinMetadataBytes)
	return err
}

// PendingJoins keeps terminal cleanup from archiving a partially applied root.
func PendingJoins(reader *journal.Reader) ([]JoinState, error) {
	states, err := ReadJoins(reader)
	if err != nil {
		return nil, err
	}
	var pending []JoinState
	for _, state := range states {
		if !state.Applied {
			pending = append(pending, *state)
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].Sequence < pending[j].Sequence })
	return pending, nil
}
