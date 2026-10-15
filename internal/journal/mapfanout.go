package journal

import (
	"encoding/json"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// MapRoster is the frozen roster a map.started event carries (design
// plan-driven-dynamic-fan-out.md §4.2, §6.2). It is the durable source of
// truth for one map visit: item branches, resume and join assembly are all
// rebuilt from it, never from the producer's current outputs.
type MapRoster struct {
	// Source identifies the exact producer attempt and artifact that were
	// frozen. Normative except for the storage metadata noted on it.
	Source FanOutSourceRef `json:"source"`
	// FailurePolicy, ItemTimeoutSeconds and MaxConcurrent are the map's
	// effective execution bounds at freeze time. MaxConcurrent is the
	// effective bound, so an unset maxConcurrent is recorded as 1. Normative.
	FailurePolicy      apiv1.BranchFailurePolicy `json:"failurePolicy"`
	ItemTimeoutSeconds int32                     `json:"itemTimeoutSeconds,omitempty"`
	MaxConcurrent      int32                     `json:"maxConcurrent"`
	// Items is the normalized roster: identities are unique and sorted by
	// bytewise UTF-8 order, and item i runs as branch i+1. An empty roster is
	// recorded as an empty array, never omitted. Normative.
	Items []MapItem `json:"items"`
}

// MapItem is one normalized roster entry.
type MapItem struct {
	// Identity is the author-chosen stable identity.
	Identity string `json:"identity"`
	// Digest is "sha256:" plus the lowercase hex SHA-256 of the RFC 8785
	// serialization of {identity, item}. The runner always computes it; a
	// digest the producer supplied was verified equal before freezing.
	Digest string `json:"digest"`
	// Item is the RFC 8785 canonical JSON object bound into the mapped task.
	Item json.RawMessage `json:"item"`
}

// FanOutSourceRef is the immutable reference to a frozen fan-out source
// document: the declared producer slot, the producer attempt the journal
// bound it to, and the artifact's content identity and integrity.
type FanOutSourceRef struct {
	// Task and Slot are the declared source (apiv1.FanOutSource).
	Task string `json:"task"`
	Slot string `json:"slot"`
	// Visit is the producer's journal-bound stage.started sequence (the
	// artifact publication visit): runner-local provenance, excluded from
	// conformance as a selection's is. Attempt is the producer attempt number
	// and is normative.
	Visit   uint64 `json:"visit"`
	Attempt int    `json:"attempt"`
	// Digest and Integrity are the source artifact's content digest and
	// provenance grade. Mapped bindings inherit Integrity. Normative.
	Digest    string          `json:"digest"`
	Integrity apiv1.Integrity `json:"integrity"`
	// Path, Size and MediaType locate the stored artifact. Excluded from
	// conformance, like Ref's storage metadata.
	Path      string `json:"path,omitempty"`
	Size      int64  `json:"size,omitempty"`
	MediaType string `json:"mediaType,omitempty"`
}
