package recovery

// Provenance is the exact retained-state evidence a workflow stage reports for
// the source run whose implementation it adopted or skipped. It lets later
// stages and operators bind a continuation to the precise retained snapshot
// rather than to a run identifier alone. Fields absent from the source record
// are omitted, never inferred.
type Provenance struct {
	// SourceRecoveryRef is the retained ref holding the source run's snapshot.
	SourceRecoveryRef string `json:"sourceRecoveryRef,omitempty"`
	// SourceBaseRef is the remote base ref the source run selected; version-1
	// records predate it, so it is omitted for them.
	SourceBaseRef string `json:"sourceBaseRef,omitempty"`
	// SourceBaseSHA is the commit the source run's implementation was based on.
	SourceBaseSHA string `json:"sourceBaseSha,omitempty"`
	// SourceSnapshotSHA is the retained head commit of the source run.
	SourceSnapshotSHA string `json:"sourceSnapshotSha,omitempty"`
	// SourcePatchDigest is the digest of the exact retained patch bytes.
	SourcePatchDigest string `json:"sourcePatchDigest,omitempty"`
}

// Provenance returns the record's source evidence, or nil when the record does
// not identify a source run (no retained implementation was selected).
func (r Record) Provenance() *Provenance {
	if r.RunID == "" {
		return nil
	}
	return &Provenance{
		SourceRecoveryRef: r.Ref,
		SourceBaseRef:     r.BaseRef,
		SourceBaseSHA:     r.BaseSHA,
		SourceSnapshotSHA: r.SnapshotSHA,
		SourcePatchDigest: r.PatchDigest,
	}
}
