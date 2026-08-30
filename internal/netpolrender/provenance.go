package netpolrender

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Provenance drift check (issue #3568 must-carry control 1): every allowlist
// group that declares an upstream source carries the sha256 of the upstream
// document its CIDRs were transcribed from. CheckProvenance re-fetches each
// distinct source and compares EVERY group's marker — not just the first.
// The first-only variant was a real defect (silent narrowing: groups past the
// first could rot forever), fixed upstream in Goobernetes-Infra and pinned
// here by TestCheckProvenanceValidatesEveryMarker. Without this check a stale
// CIDR set fails in production as a mid-run connect timeout, which reads
// exactly like a correct policy denial.

// Fetcher fetches one provenance source document. It is a seam so tests (and
// air-gapped operators) can substitute the transport.
type Fetcher func(ctx context.Context, url string) ([]byte, error)

// ProvenanceMismatch names one group whose marker no longer matches its
// upstream document.
type ProvenanceMismatch struct {
	// Group is the allowlist group name.
	Group string
	// Source is the upstream URL.
	Source string
	// Declared is the group's recorded sha256.
	Declared string
	// Live is the sha256 of the document as fetched, empty when the fetch
	// itself failed.
	Live string
	// FetchErr is the fetch failure, if any.
	FetchErr error
}

func (m ProvenanceMismatch) String() string {
	if m.FetchErr != nil {
		return fmt.Sprintf("group %q: fetch %s: %v", m.Group, m.Source, m.FetchErr)
	}
	return fmt.Sprintf("group %q: %s rotated — declared sha256:%s, live sha256:%s — "+
		"re-transcribe the CIDRs and update sourceSHA256 (stale CIDRs fail mid-run as connect "+
		"timeouts indistinguishable from correct denials)", m.Group, m.Source, m.Declared, m.Live)
}

// CheckProvenance validates every declared provenance marker against its live
// upstream document. Groups without a Source are returned in unverifiable —
// they are operator-local sets with no upstream to drift from; the caller
// reports them rather than silently skipping. Each distinct URL is fetched
// once; every group naming it is still checked individually.
func CheckProvenance(ctx context.Context, fetch Fetcher, groups []AllowlistGroup) (mismatches []ProvenanceMismatch, unverifiable []string) {
	type fetched struct {
		sum string
		err error
	}
	byURL := make(map[string]fetched)
	for _, group := range groups {
		if group.Source == "" {
			unverifiable = append(unverifiable, group.Name)
			continue
		}
		result, done := byURL[group.Source]
		if !done {
			body, err := fetch(ctx, group.Source)
			if err != nil {
				result = fetched{err: err}
			} else {
				sum := sha256.Sum256(body)
				result = fetched{sum: hex.EncodeToString(sum[:])}
			}
			byURL[group.Source] = result
		}
		switch {
		case result.err != nil:
			mismatches = append(mismatches, ProvenanceMismatch{
				Group: group.Name, Source: group.Source, Declared: group.SourceSHA256, FetchErr: result.err,
			})
		case !strings.EqualFold(result.sum, group.SourceSHA256):
			mismatches = append(mismatches, ProvenanceMismatch{
				Group: group.Name, Source: group.Source, Declared: strings.ToLower(group.SourceSHA256), Live: result.sum,
			})
		}
	}
	sort.Strings(unverifiable)
	return mismatches, unverifiable
}
