package childworkflow

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
)

// ValidationHelp is the author-facing command's registry help text.
const ValidationHelp = "Usage: goobers workflow validate-child --gaggle <name> --parent <workflow> --stage <stage> [--backend runner|engine] [--json] <proposal.yaml> [path]\n\n" +
	"Validate one generated child Workflow against an opted-in parent stage in\n" +
	"the instance's current active config/ tree (default path \".\"). The parent\n" +
	"supplies policy; child grants are also bounded by the parent's stage capabilities.\n" +
	"The proposal must pin DSL 3.1 and declare only a plain manual trigger.\n\n" +
	"This is advisory current-config validation, not durable run admission. It\n" +
	"does not start or queue work, resolve credentials, invoke a model, or refresh\n" +
	"workflowSource. --backend selects the validation target (default runner).\n" +
	"Config digests identify declarative validation inputs, not an execution archive.\n" +
	"--json emits child-workflow-validation/v1 diagnostics and digests.\n" +
	"Exit codes: 0 = valid, 1 = invalid proposal/configuration, 2 = usage/IO error.\n"

// RunValidationCLI receives the host's observable flag set and offline Goober
// admission so command discovery and runtime harness contracts stay shared.
func RunValidationCLI(args []string, fs *flag.FlagSet, stdout, stderr io.Writer, admit GooberAdmission) int {
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = io.WriteString(stderr, ValidationHelp) }
	gaggle := fs.String("gaggle", "", "configured parent gaggle (required)")
	parent := fs.String("parent", "", "configured parent workflow (required)")
	stage := fs.String("stage", "", "configured parent agentic stage (required)")
	backend := fs.String("backend", string(BackendRunner), "advisory validation target: runner or engine")
	asJSON := fs.Bool("json", false, "emit structured diagnostics and digests")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *gaggle == "" || *parent == "" || *stage == "" || fs.NArg() < 1 || fs.NArg() > 2 || (*backend != string(BackendRunner) && *backend != string(BackendEngine)) {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}
	report, err := ValidateConfigured(ConfiguredRequest{Root: root, Gaggle: *gaggle, ParentWorkflow: *parent, ParentStage: *stage, ProposalPath: fs.Arg(0), Backend: Backend(*backend)}, admit)
	code := 0
	if err != nil {
		report = configuredRefusal(report, "io", err.Error())
		code = 2
	} else if !report.Valid {
		code = 1
	}
	if err := renderConfiguredReport(stdout, report, *asJSON); err != nil {
		_, _ = fmt.Fprintf(stderr, "error: write validation report: %v\n", err)
		return 2
	}
	return code
}

func renderConfiguredReport(output io.Writer, report ConfiguredReport, asJSON bool) error {
	if asJSON {
		encoder := json.NewEncoder(output)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	state := "INVALID"
	if report.Valid {
		state = "VALID"
	}
	if _, err := fmt.Fprintf(output, "%s child proposal — advisory current-config validation\nParent: %s/%s stage %s; backend: %s\n", state, report.Parent.Gaggle, report.Parent.Workflow, report.Parent.Stage, report.Backend); err != nil {
		return err
	}
	for _, diagnostic := range report.Diagnostics {
		if _, err := fmt.Fprintf(output, "  %s %s %s: %s\n", diagnostic.Code, diagnostic.Stage, diagnostic.Field, diagnostic.Message); err != nil {
			return err
		}
	}
	for _, warning := range report.Warnings {
		if _, err := fmt.Fprintf(output, "  %s\n", warning); err != nil {
			return err
		}
	}
	if report.Valid {
		_, err := fmt.Fprintf(output, "Source: %s\nCanonical: %s\nConfig inputs: %s\nPolicy: %s\nWorkflow: %s\n", report.SourceDigest, report.CanonicalDigest, report.ConfigDigest, report.PolicyDigest, report.WorkflowDigest)
		return err
	}
	return nil
}
