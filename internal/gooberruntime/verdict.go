// Package gooberruntime carries the verdict-validation rules of the retired
// per-run agent runtime.
//
// The runtime itself (environment preparation, Copilot harness driving, the
// invoke.Goober implementation) was superseded by the local runner's stage
// execution (the `goobers` binary, via internal/harness) and deleted together
// with its host binary, cmd/goober-runtime, per goobernetes-architecture.md
// D5/§4 (#2055 resolved: supersede). What remains is the still-live envelope
// contract enforcement consumed by cmd/goobers.
package gooberruntime

import (
	"fmt"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func validateVerdict(verdict apiv1.Verdict) error {
	if !verdict.Decision.IsValid() {
		return fmt.Errorf("invalid verdict decision %q", verdict.Decision)
	}
	for i, finding := range verdict.Findings {
		if !validSeverity(finding.Severity) {
			return fmt.Errorf("finding[%d].severity %q is invalid", i, finding.Severity)
		}
		if strings.TrimSpace(finding.Message) == "" {
			return fmt.Errorf("finding[%d].message is required", i)
		}
		if !finding.IsValid() {
			return fmt.Errorf("finding[%d] is invalid (class %q, blockingPRs %v)", i, finding.Class, finding.BlockingPRs)
		}
	}
	return nil
}

// ValidateMergeReviewVerdict applies the ordinary verdict rules and requires
// every finding to carry a routing class.
func ValidateMergeReviewVerdict(verdict apiv1.Verdict) error {
	if err := validateVerdict(verdict); err != nil {
		return err
	}
	for i, finding := range verdict.Findings {
		if !finding.Class.IsValid() {
			return fmt.Errorf("finding[%d].class %q is invalid", i, finding.Class)
		}
	}
	return nil
}

func validSeverity(severity apiv1.Severity) bool {
	switch severity {
	case apiv1.SeverityInfo, apiv1.SeverityWarning, apiv1.SeverityError, apiv1.SeverityCritical:
		return true
	default:
		return false
	}
}
