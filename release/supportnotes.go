package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/goobers/goobers/internal/supportmatrix"
)

const (
	supportSnapshotSchemaVersion = 1
	supportSnapshotFile          = "dsl-support-matrix.json"
)

type supportSnapshot struct {
	SchemaVersion int                     `json:"schemaVersion"`
	Release       string                  `json:"release"`
	Versions      []supportmatrix.Version `json:"versions"`
}

type supportDelta struct {
	NewlyDeprecated  []supportmatrix.Version
	NewlyUnsupported []supportmatrix.Version
	// NewlyRetracted lists versions whose Retraction (#4708) first appears in
	// this release — a previously published unsupportedAfter commitment
	// deliberately withdrawn, surfaced here so a consumer of the release
	// notes sees the commitment changed and why, not just the matrix's raw
	// diff.
	NewlyRetracted []supportmatrix.Version
}

// gitDescribeAheadOfTagSuffix matches the trailing shape `git describe`
// appends when HEAD is NOT exactly on a tag: `-<commit count>-g<short hash>`,
// optionally followed by `-dirty`. It is a literal string-shape check, not a
// git query, so classifying a version this way is itself deterministic and
// unit-testable.
var gitDescribeAheadOfTagSuffix = regexp.MustCompile(`-[0-9]+-g[0-9a-f]+(-dirty)?$`)

// checkSupportMatrixForRelease refuses to package a tagged release whose
// compiled-in DSL support matrix declares a level the release does not actually
// reach — a level whose lifecycle transition is dated at a release that has not
// happened yet (#4215). Prereleases are checked against their stable release
// line too: an RC must not bypass the gate that its final release will face.
// Unversioned development builds have no release line to check.
//
// A version several commits past a tag (git describe's own shape for "not
// exactly on a tag") is likewise skipped: it names an ordinary development
// build, not an intended release, and checking it would refuse based on
// whichever tag the checkout happens to sit nearest — the exact bug #4663
// reports, where a checkout hundreds of commits past a stale stable tag
// spuriously failed against that tag's release line. Whether a lifecycle
// transition can actually ship is asserted independently of checkout
// position by TestDSLMatrixAgainstNextPlannedRelease (#4709), against the
// declared supportmatrix.NextPlannedRelease, on every PR.
func checkSupportMatrixForRelease(version string) error {
	version = strings.TrimSpace(version)
	if gitDescribeAheadOfTagSuffix.MatchString(version) {
		return nil
	}
	releaseLine, _, _ := strings.Cut(version, "-")
	if !isFinalReleaseVersion(releaseLine) {
		return nil
	}
	if err := supportmatrix.ValidateSupportPolicyForRelease(supportmatrix.GetDSL(), releaseLine); err != nil {
		return fmt.Errorf("DSL support matrix cannot ship in %s: %w", version, err)
	}
	return nil
}

func isFinalReleaseVersion(version string) bool {
	if !strings.HasPrefix(version, "v") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return false
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

func supportReleaseMetadata(version, previousPath string) (string, []byte, error) {
	current, err := newSupportSnapshot(version, supportmatrix.GetDSL())
	if err != nil {
		return "", nil, err
	}

	var previous *supportSnapshot
	if previousPath != "" {
		snapshot, err := readSupportSnapshot(previousPath)
		if err != nil {
			return "", nil, fmt.Errorf("read previous support matrix: %w", err)
		}
		previous = &snapshot
	}

	notes, err := renderSupportDelta(current, previous)
	if err != nil {
		return "", nil, fmt.Errorf("render support-matrix delta: %w", err)
	}
	snapshotJSON, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return "", nil, fmt.Errorf("encode support matrix: %w", err)
	}
	snapshotJSON = append(snapshotJSON, '\n')
	return notes, snapshotJSON, nil
}

func newSupportSnapshot(release string, matrix supportmatrix.SupportMatrix) (supportSnapshot, error) {
	snapshot := supportSnapshot{
		SchemaVersion: supportSnapshotSchemaVersion,
		Release:       strings.TrimSpace(release),
		Versions:      matrix.Versions(),
	}
	if err := validateSupportSnapshot(snapshot); err != nil {
		return supportSnapshot{}, err
	}
	return snapshot, nil
}

func readSupportSnapshot(path string) (supportSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return supportSnapshot{}, err
	}
	var snapshot supportSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return supportSnapshot{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := validateSupportSnapshot(snapshot); err != nil {
		return supportSnapshot{}, fmt.Errorf("validate %s: %w", path, err)
	}
	return snapshot, nil
}

func validateSupportSnapshot(snapshot supportSnapshot) error {
	if snapshot.SchemaVersion != supportSnapshotSchemaVersion {
		return fmt.Errorf("unsupported support snapshot schema version %d", snapshot.SchemaVersion)
	}
	if strings.TrimSpace(snapshot.Release) == "" {
		return fmt.Errorf("support snapshot release must not be empty")
	}
	if len(snapshot.Versions) == 0 {
		return fmt.Errorf("support snapshot must contain at least one DSL version")
	}
	seen := make(map[string]struct{}, len(snapshot.Versions))
	for _, version := range snapshot.Versions {
		if strings.TrimSpace(version.Version) == "" {
			return fmt.Errorf("support snapshot contains an empty DSL version")
		}
		switch version.Level {
		case supportmatrix.LevelPreview,
			supportmatrix.LevelSupported,
			supportmatrix.LevelDeprecated,
			supportmatrix.LevelUnsupported:
		default:
			return fmt.Errorf("DSL version %q has invalid support level %q", version.Version, version.Level)
		}
		if _, ok := seen[version.Version]; ok {
			return fmt.Errorf("support snapshot contains duplicate DSL version %q", version.Version)
		}
		seen[version.Version] = struct{}{}
	}
	return nil
}

func supportMatrixDelta(previous, current supportSnapshot) (supportDelta, error) {
	previousLevels := make(map[string]supportmatrix.Level, len(previous.Versions))
	previousRetractions := make(map[string]*supportmatrix.Retraction, len(previous.Versions))
	for _, version := range previous.Versions {
		previousLevels[version.Version] = version.Level
		previousRetractions[version.Version] = version.Retraction
	}

	var delta supportDelta
	for _, version := range current.Versions {
		if version.Retraction != nil && previousRetractions[version.Version] == nil {
			delta.NewlyRetracted = append(delta.NewlyRetracted, version)
		}
		if previousLevels[version.Version] == version.Level {
			continue
		}
		switch version.Level {
		case supportmatrix.LevelDeprecated:
			delta.NewlyDeprecated = append(delta.NewlyDeprecated, version)
		case supportmatrix.LevelUnsupported:
			delta.NewlyUnsupported = append(delta.NewlyUnsupported, version)
		default:
			continue
		}
		if strings.TrimSpace(version.Replacement) == "" {
			return supportDelta{}, fmt.Errorf("DSL version %q became %s without a replacement", version.Version, version.Level)
		}
	}
	return delta, nil
}

func renderSupportDelta(current supportSnapshot, previous *supportSnapshot) (string, error) {
	var b strings.Builder
	b.WriteString("## DSL support-matrix delta\n\n")
	if previous == nil {
		fmt.Fprintf(&b, "Release `%s` records the first DSL support matrix; there is no previous release to compare.\n", current.Release)
		return b.String(), nil
	}

	delta, err := supportMatrixDelta(*previous, current)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(&b, "Compared with `%s`.\n\n", previous.Release)
	if len(delta.NewlyDeprecated) == 0 && len(delta.NewlyUnsupported) == 0 && len(delta.NewlyRetracted) == 0 {
		b.WriteString("No DSL versions became deprecated or unsupported in this release.\n")
		return b.String(), nil
	}
	writeSupportChanges(&b, "Newly deprecated", delta.NewlyDeprecated)
	writeSupportChanges(&b, "Newly unsupported", delta.NewlyUnsupported)
	writeRetractionChanges(&b, delta.NewlyRetracted)
	return b.String(), nil
}

// writeRetractionChanges surfaces a declared retraction (#4708) in the
// release notes: the withdrawn commitment and why, so a consumer of the
// support-matrix delta sees the promise changed rather than only inferring
// it from an unexplained effectiveIn value in the raw snapshot.
func writeRetractionChanges(b *strings.Builder, versions []supportmatrix.Version) {
	if len(versions) == 0 {
		return
	}
	b.WriteString("### Retracted commitments\n\n")
	for _, version := range versions {
		r := version.Retraction
		fmt.Fprintf(b, "- DSL `%s`: the unsupported-after `%s` published for this version is retracted in `%s`. %s\n",
			version.Version, r.UnsupportedAfter, r.Release, r.Rationale)
	}
	b.WriteString("\n")
}

func writeSupportChanges(b *strings.Builder, heading string, versions []supportmatrix.Version) {
	if len(versions) == 0 {
		return
	}
	fmt.Fprintf(b, "### %s\n\n", heading)
	for _, version := range versions {
		fmt.Fprintf(b, "- DSL `%s` is now `%s`; migrate with `goobers fix --to %s`",
			version.Version, version.Level, version.Replacement)
		if version.UnsupportedAfter != "" {
			fmt.Fprintf(b, " before `%s`", version.UnsupportedAfter)
		}
		b.WriteString(".\n")
	}
	b.WriteString("\n")
}
