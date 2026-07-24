package k8spreflight

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// WriteText renders the report as the human-readable conformance table:
// per-check status rows with shape-doc citations, remediation hints for every
// non-pass row, and a one-line verdict.
func WriteText(w io.Writer, r Report) {
	pf := func(dst io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(dst, format, a...) }

	pf(w, "cluster preflight — docs/design/k8s-infra-shape.md conformance\n")
	if r.Target != "" {
		pf(w, "target: %s\n", r.Target)
	}
	pf(w, "\n")

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	pf(tw, "  STATUS\tSEVERITY\tCHECK\tSECTION\tDETAIL\n")
	for _, result := range r.Results {
		pf(tw, "  %s\t%s\t%s\t%s\t%s\n",
			strings.ToUpper(string(result.Status)),
			result.Severity,
			result.ID,
			result.Citation,
			result.Detail,
		)
	}
	_ = tw.Flush()

	var hints bool
	for _, result := range r.Results {
		if result.Status == StatusPass || result.Hint == "" {
			continue
		}
		if !hints {
			pf(w, "\nremediation:\n")
			hints = true
		}
		pf(w, "  %s: %s\n", result.ID, result.Hint)
	}

	var pass, warn, fail int
	for _, result := range r.Results {
		switch result.Status {
		case StatusPass:
			pass++
		case StatusWarn:
			warn++
		case StatusFail:
			fail++
		}
	}
	verdict := "cluster conforms to the documented shape"
	if !r.Conformant {
		verdict = "cluster does NOT conform — a required check failed"
	}
	pf(w, "\n%d pass, %d warn, %d fail — %s\n", pass, warn, fail, verdict)
}

// WriteJSON emits the report as indented JSON — the stable machine-readable
// form behind --report json.
func WriteJSON(w io.Writer, r Report) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(r)
}
