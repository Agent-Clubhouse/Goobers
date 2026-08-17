package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
)

const (
	verdictHumanRationaleLimit = 4096
	verdictHumanFindingLimit   = 50
	verdictHumanLocationLimit  = 1024
	verdictHumanMessageLimit   = 1024
)

type verdictView struct {
	Gate          string                        `json:"gate"`
	Decision      string                        `json:"decision"`
	Target        string                        `json:"target"`
	Rationale     string                        `json:"rationale,omitempty"`
	Findings      []apiv1.Finding               `json:"findings"`
	Cached        bool                          `json:"cached"`
	DiffDigest    string                        `json:"diffDigest,omitempty"`
	Artifact      *readservice.ArtifactMetadata `json:"artifact,omitempty"`
	ArtifactError string                        `json:"artifactError,omitempty"`
	Content       *apiv1.Verdict                `json:"content,omitempty"`
}

func loadVerdictViews(
	ctx context.Context,
	reads readservice.OfflineRuns,
	runID string,
	events []readservice.RunEvent,
) []verdictView {
	verdicts := make([]verdictView, 0)
	for _, event := range events {
		if !event.KnownSchema || event.Type != journal.EventGateEvaluated {
			continue
		}
		view := verdictView{
			Gate:       event.Gate,
			Decision:   event.Verdict,
			Target:     event.Target,
			Findings:   []apiv1.Finding{},
			Cached:     runnerBool(event.Runner, "verdictCacheHit"),
			DiffDigest: runnerString(event.Runner, "diffDigest"),
		}
		artifact := event.Artifact
		if artifact == nil && event.JournalEvent != nil && event.JournalEvent.Ref != nil {
			ref := event.JournalEvent.Ref
			artifact = &readservice.ArtifactMetadata{
				Name:      event.Name,
				Digest:    ref.Digest,
				Size:      ref.Size,
				MediaType: ref.MediaType,
			}
		}
		if artifact == nil {
			if event.Name != "" {
				view.ArtifactError = "verdict artifact reference is missing"
			}
			verdicts = append(verdicts, view)
			continue
		}

		metadata := *artifact
		view.Artifact = &metadata
		content, err := reads.Artifact(ctx, runID, metadata.Digest)
		if err != nil {
			view.ArtifactError = err.Error()
			verdicts = append(verdicts, view)
			continue
		}
		var verdict apiv1.Verdict
		if err := json.Unmarshal(content.Bytes, &verdict); err != nil {
			view.ArtifactError = fmt.Sprintf("decode verdict artifact: %v", err)
			verdicts = append(verdicts, view)
			continue
		}
		view.Content = &verdict
		view.Rationale = verdict.Rationale
		view.Findings = verdict.Findings
		verdicts = append(verdicts, view)
	}
	return verdicts
}

func runnerBool(values map[string]any, key string) bool {
	value, _ := values[key].(bool)
	return value
}

func runnerString(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return value
}

func renderVerdicts(stdout io.Writer, verdicts []verdictView) {
	pln(stdout, "verdicts:")
	if len(verdicts) == 0 {
		pln(stdout, "  (none recorded)")
		return
	}
	for _, verdict := range verdicts {
		pf(stdout, "  gate=%s decision=%s target=%s cached=%t", verdict.Gate, verdict.Decision, verdict.Target, verdict.Cached)
		if verdict.DiffDigest != "" {
			pf(stdout, " diffDigest=%s", verdict.DiffDigest)
		}
		pln(stdout, "")
		if verdict.ArtifactError != "" {
			pf(stdout, "    artifact: unavailable (%s)\n", singleLine(verdict.ArtifactError))
		}
		if verdict.Rationale != "" {
			pf(stdout, "    rationale: %s\n", indentContinuation(truncateHuman(verdict.Rationale, verdictHumanRationaleLimit), "      "))
		}
		limit := len(verdict.Findings)
		if limit > verdictHumanFindingLimit {
			limit = verdictHumanFindingLimit
		}
		for _, finding := range verdict.Findings[:limit] {
			location := ""
			if finding.Location != "" {
				location = " location=" + truncateHuman(singleLine(finding.Location), verdictHumanLocationLimit)
			}
			pf(stdout, "    finding: severity=%s%s message=%s\n",
				finding.Severity, location, indentContinuation(truncateHuman(finding.Message, verdictHumanMessageLimit), "      "))
		}
		if omitted := len(verdict.Findings) - limit; omitted > 0 {
			pf(stdout, "    ... %d additional findings truncated\n", omitted)
		}
	}
}

func truncateHuman(value string, maxRunes int) string {
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "... [truncated]"
}

func indentContinuation(value, indent string) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.ReplaceAll(value, "\n", "\n"+indent)
}

func singleLine(value string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(value)
}
