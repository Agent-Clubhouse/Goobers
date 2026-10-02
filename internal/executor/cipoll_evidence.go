package executor

import (
	"strings"

	"github.com/goobers/goobers/providers"
)

// ciCheckEvidence returns the annotations and a fallback summary for the
// check at index i of the polled checks. The summary is used only when the
// check carries none of its own.
type ciCheckEvidence func(i int, check providers.CheckDetail) ([]providers.CheckAnnotation, string)

// ciEvidenceByName resolves evidence by check name, for a provider whose
// failure evidence carries the polled check names (GitHub, Gitea).
func ciEvidenceByName(annotations map[string][]providers.CheckAnnotation) ciCheckEvidence {
	return func(_ int, check providers.CheckDetail) ([]providers.CheckAnnotation, string) {
		return annotations[check.Name], ""
	}
}

// ciEvidenceForPullRequest pairs pull-request-scoped failure evidence with
// the polled failing checks (#5652). Both come from the same policy
// evaluations in the same order, but the evidence names a policy more
// precisely than the poll does ("Build: ci" for the polled "Build"), and
// several policies can share a polled name. So each failing check, in order,
// takes the first unused failure with the same build link, else the first
// unused failure whose name is the check's name or refines it.
func ciEvidenceForPullRequest(checks []providers.CheckDetail, failures []providers.CIFailureDetail) ciCheckEvidence {
	matched := make(map[int]providers.CIFailureDetail, len(failures))
	used := make([]bool, len(failures))
	for i, check := range checks {
		if check.State != providers.CheckStateFailing {
			continue
		}
		if j := matchCIFailure(check, failures, used); j >= 0 {
			used[j] = true
			matched[i] = failures[j]
		}
	}
	return func(i int, _ providers.CheckDetail) ([]providers.CheckAnnotation, string) {
		failure, ok := matched[i]
		if !ok {
			return nil, ""
		}
		return failure.Annotations, failure.Summary
	}
}

func matchCIFailure(check providers.CheckDetail, failures []providers.CIFailureDetail, used []bool) int {
	if check.URL != "" {
		for j, failure := range failures {
			if !used[j] && failure.URL == check.URL {
				return j
			}
		}
	}
	for j, failure := range failures {
		if !used[j] && ciFailureNameRefines(failure.Name, check.Name) {
			return j
		}
	}
	return -1
}

// ciFailureNameRefines reports whether name is check, or check qualified by a
// policy name (": ...") or configuration id (" #...").
func ciFailureNameRefines(name, check string) bool {
	return name == check || strings.HasPrefix(name, check+": ") || strings.HasPrefix(name, check+" #")
}
