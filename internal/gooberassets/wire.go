package gooberassets

import "io/fs"

// wire.go gives a Bundle a serializable form so it can travel to a process that
// cannot read the instance's asset directory.
//
// A mode-3 stage pod is exactly that process: it has no instance root by
// design, so an agentic stage's assets must arrive as DATA rather than be
// loaded from disk. A Bundle already holds every file's bytes in memory (Load
// reads contents), so this exposes what is already there rather than changing
// what a Bundle is.
//
// Fingerprint() is deliberately unchanged and remains the identity: a bundle
// that survives a round trip must fingerprint identically, which is what
// TestWireRoundTripPreservesFingerprint asserts. That makes the transport
// verifiable — the receiver can prove it reconstructed the same bundle the
// sender had, rather than trusting the channel.

// WireEntry is one file or directory in a serialized bundle.
type WireEntry struct {
	Path string      `json:"path"`
	Mode fs.FileMode `json:"mode"`
	Data []byte      `json:"data,omitempty"`
	Dir  bool        `json:"dir,omitempty"`
}

// WireBundle is the serializable form of a Bundle.
type WireBundle struct {
	RootMode fs.FileMode `json:"rootMode"`
	Entries  []WireEntry `json:"entries"`
}

// ToWire renders the bundle for transport. A nil bundle renders as nil so a
// goober with no assets travels as an absent value rather than an empty one.
func (b *Bundle) ToWire() *WireBundle {
	if b == nil {
		return nil
	}
	out := &WireBundle{RootMode: b.rootMode, Entries: make([]WireEntry, 0, len(b.entries))}
	for _, e := range b.entries {
		out.Entries = append(out.Entries, WireEntry{Path: e.path, Mode: e.mode, Data: e.data, Dir: e.dir})
	}
	return out
}

// FromWire reconstructs a bundle received over the wire.
func FromWire(w *WireBundle) *Bundle {
	if w == nil {
		return nil
	}
	b := &Bundle{rootMode: w.RootMode, entries: make([]entry, 0, len(w.Entries))}
	for _, e := range w.Entries {
		b.entries = append(b.entries, entry{path: e.Path, mode: e.Mode, data: e.Data, dir: e.Dir})
	}
	return b
}
