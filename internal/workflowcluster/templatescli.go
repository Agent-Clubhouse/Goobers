package workflowcluster

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxTemplateEvidenceBytes bounds the joined evidence file read by --templates.
const maxTemplateEvidenceBytes = 32 << 20

// templatesArgsInvalid reports whether --templates is combined with anything
// other than --cluster-model, --json, and their values. Both template inputs
// are required together.
func templatesArgsInvalid(fs *flag.FlagSet, evidencePath, modelPath string) bool {
	invalid := evidencePath == "" || modelPath == "" || fs.NArg() > 0
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "templates", "cluster-model", "json":
		default:
			invalid = true
		}
	})
	return invalid
}

// readTemplateEvidence reads a JSON array of CandidateEvidence from path.
func readTemplateEvidence(path string) ([]CandidateEvidence, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	raw, err := io.ReadAll(io.LimitReader(file, maxTemplateEvidenceBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxTemplateEvidenceBytes {
		return nil, fmt.Errorf("template evidence exceeds %d bytes", maxTemplateEvidenceBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var evidence []CandidateEvidence
	if err := decoder.Decode(&evidence); err != nil {
		return nil, fmt.Errorf("template evidence: %w", err)
	}
	return evidence, nil
}

// runTemplatesCLI prints the advisory, review-required template candidate
// report for a frozen structural model and its joined evidence. It never
// writes config or registers templates.
func runTemplatesCLI(evidencePath, modelPath string, asJSON bool, stdout, stderr io.Writer) int {
	model, err := LoadModel(modelPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: load cluster model: %v\n", err)
		return 2
	}
	evidence, err := readTemplateEvidence(evidencePath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 2
	}
	report, err := ProposeTemplates(model, evidence, CandidateOptions{})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	if asJSON {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			_, _ = fmt.Fprintf(stderr, "error: %v\n", err)
			return 2
		}
		return 0
	}
	writeTemplateReport(stdout, model.ID, report)
	return 0
}

func writeTemplateReport(w io.Writer, modelID string, r CandidateReport) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	p("Labs: experimental, advisory reusable-template candidates (review required; nothing is registered).\n")
	p("model %s (%d candidates, %d suppressed; bar: stability %.2f, %d parent runs, %d repositories)\n",
		modelID, len(r.Candidates), len(r.Suppressed), r.Options.MinStability, r.Options.MinParentRuns, r.Options.MinRepositories)
	for _, c := range r.Candidates {
		p("\n%s  template %s  stability %.2f\n", c.ClusterID, c.Proposal.TemplateName, c.Stability)
		p("  source    %s (%s)\n", c.Proposal.SourceProposalID, c.Proposal.SourceProposalDigest)
		names := make([]string, len(c.Proposal.Parameters))
		for i, param := range c.Proposal.Parameters {
			names[i] = param.Name
		}
		p("  params    %s\n", strings.Join(names, ", "))
		p("  outcomes  %d runs at %.2f vs %d alternatives at %.2f (cohorts: %s)\n",
			c.Outcomes.CandidateRuns, c.Outcomes.CandidateRate, c.Outcomes.AlternativeRuns, c.Outcomes.AlternativeRate,
			strings.Join(c.Outcomes.TaskCohorts, ", "))
		p("  propose   %s -> %s\n", c.Proposal.Kind, c.Proposal.Target)
	}
	if len(r.Suppressed) > 0 {
		p("\nSuppressed\n")
		for _, s := range r.Suppressed {
			p("  %s  %s: %s\n", s.ClusterID, s.Reason, s.Detail)
		}
	}
}
