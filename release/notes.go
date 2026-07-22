package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/goobers/goobers/internal/workflow"
)

const (
	featureSnapshotSchemaVersion = 1
	featureSnapshotFile          = "feature-registry.json"
	releaseNotesFile             = "RELEASE_NOTES.md"
)

//go:embed release-notes.tmpl.md
var releaseNotesTemplate string

var parsedReleaseNotesTemplate = template.Must(template.New(releaseNotesFile).Parse(releaseNotesTemplate))

type featureSnapshot struct {
	SchemaVersion int                `json:"schemaVersion"`
	Release       string             `json:"release"`
	Features      []workflow.Feature `json:"features"`
}

type featureDelta struct {
	NewlyGA         []workflow.Feature
	NewlyDeprecated []workflow.Feature
	Removed         []workflow.Feature
}

type releaseNotesData struct {
	Version         string
	PreviousRelease string
	Delta           featureDelta
}

func writeReleaseMetadata(version, previousPath, outDir string) (notesPath, snapshotPath string, err error) {
	current, err := newFeatureSnapshot(version, workflow.AllFeatures())
	if err != nil {
		return "", "", err
	}

	var previous *featureSnapshot
	if previousPath != "" {
		snapshot, err := readFeatureSnapshot(previousPath)
		if err != nil {
			return "", "", fmt.Errorf("read previous feature snapshot: %w", err)
		}
		previous = &snapshot
	}

	notes, err := renderReleaseNotes(current, previous)
	if err != nil {
		return "", "", fmt.Errorf("render release notes: %w", err)
	}
	snapshotJSON, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return "", "", fmt.Errorf("encode feature snapshot: %w", err)
	}
	snapshotJSON = append(snapshotJSON, '\n')

	snapshotPath = filepath.Join(outDir, featureSnapshotFile)
	if err := os.WriteFile(snapshotPath, snapshotJSON, 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", snapshotPath, err)
	}
	notesPath = filepath.Join(outDir, releaseNotesFile)
	if err := os.WriteFile(notesPath, []byte(notes), 0o644); err != nil {
		return "", "", fmt.Errorf("write %s: %w", notesPath, err)
	}
	return notesPath, snapshotPath, nil
}

func newFeatureSnapshot(release string, features []workflow.Feature) (featureSnapshot, error) {
	snapshot := featureSnapshot{
		SchemaVersion: featureSnapshotSchemaVersion,
		Release:       strings.TrimSpace(release),
		Features:      append([]workflow.Feature(nil), features...),
	}
	if err := validateFeatureSnapshot(snapshot); err != nil {
		return featureSnapshot{}, err
	}
	sort.Slice(snapshot.Features, func(i, j int) bool {
		return snapshot.Features[i].ID < snapshot.Features[j].ID
	})
	return snapshot, nil
}

func readFeatureSnapshot(path string) (featureSnapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return featureSnapshot{}, err
	}
	var snapshot featureSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return featureSnapshot{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if err := validateFeatureSnapshot(snapshot); err != nil {
		return featureSnapshot{}, fmt.Errorf("validate %s: %w", path, err)
	}
	return snapshot, nil
}

func validateFeatureSnapshot(snapshot featureSnapshot) error {
	if snapshot.SchemaVersion != featureSnapshotSchemaVersion {
		return fmt.Errorf("unsupported feature snapshot schema version %d", snapshot.SchemaVersion)
	}
	if strings.TrimSpace(snapshot.Release) == "" {
		return fmt.Errorf("feature snapshot release must not be empty")
	}
	if len(snapshot.Features) == 0 {
		return fmt.Errorf("feature snapshot must contain at least one feature")
	}
	if _, err := workflow.NewFeatureRegistry(snapshot.Features); err != nil {
		return fmt.Errorf("invalid feature registry: %w", err)
	}
	return nil
}

func featureSupportDelta(previous, current []workflow.Feature) (featureDelta, error) {
	previousLevels := make(map[workflow.FeatureID]workflow.SupportLevel, len(previous))
	for _, feature := range previous {
		previousLevels[feature.ID] = feature.Level
	}
	currentIDs := make(map[workflow.FeatureID]struct{}, len(current))
	for _, feature := range current {
		currentIDs[feature.ID] = struct{}{}
	}
	var dropped []string
	for _, feature := range previous {
		if _, ok := currentIDs[feature.ID]; !ok {
			dropped = append(dropped, string(feature.ID))
		}
	}
	if len(dropped) > 0 {
		sort.Strings(dropped)
		return featureDelta{}, fmt.Errorf("current feature registry dropped %s; retain each entry at support level %q", strings.Join(dropped, ", "), workflow.SupportRemoved)
	}

	var delta featureDelta
	for _, feature := range current {
		if previousLevels[feature.ID] == feature.Level {
			continue
		}
		switch feature.Level {
		case workflow.SupportGA:
			delta.NewlyGA = append(delta.NewlyGA, feature)
		case workflow.SupportDeprecated:
			delta.NewlyDeprecated = append(delta.NewlyDeprecated, feature)
		case workflow.SupportRemoved:
			delta.Removed = append(delta.Removed, feature)
		}
	}
	sortFeatures(delta.NewlyGA)
	sortFeatures(delta.NewlyDeprecated)
	sortFeatures(delta.Removed)
	return delta, nil
}

func sortFeatures(features []workflow.Feature) {
	sort.Slice(features, func(i, j int) bool {
		return features[i].ID < features[j].ID
	})
}

func renderReleaseNotes(current featureSnapshot, previous *featureSnapshot) (string, error) {
	data := releaseNotesData{
		Version: current.Release,
	}
	if previous != nil {
		data.PreviousRelease = previous.Release
	}
	var previousFeatures []workflow.Feature
	if previous != nil {
		previousFeatures = previous.Features
	}
	delta, err := featureSupportDelta(previousFeatures, current.Features)
	if err != nil {
		return "", err
	}
	data.Delta = delta

	var rendered bytes.Buffer
	if err := parsedReleaseNotesTemplate.Execute(&rendered, data); err != nil {
		return "", err
	}
	return rendered.String(), nil
}
