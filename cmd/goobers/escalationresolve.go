package main

import (
	"flag"
	"io"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

// escalationresolve.go gives the daemon's HITL escalation-resolution route
// (apicontract.RouteResolveEscalation) a CLI caller (#3807). The route and its
// handler already existed; without a verb the only way to resolve an escalated
// run remotely was to construct the request by hand, so operating a clustered
// instance meant exec-ing into the daemon's own pod. Like the other
// intervention verbs it speaks the API rather than the filesystem, so --api
// (or $GOOBERS_DAEMON_API) reaches a daemon that shares neither.

const escalationsResolveHelp = "Usage: goobers escalations resolve --resolution=approve|deny|redirect [--gate=<gate>] [--decision=<name>]\n" +
	"                                  [--rationale=<text>] [--actor=<identity>] [--api=<url>] <run-id> [path]\n\n" +
	"Resolve an escalated run through the daemon's HITL plane: approve resumes\n" +
	"it through the escalated gate's pass branch, redirect resumes it through a\n" +
	"chosen decision branch, and deny records the escalation resolved-denied and\n" +
	"leaves the run terminal. approve and redirect name the escalated gate with\n" +
	"--gate; redirect also names the branch with --decision. deny and redirect\n" +
	"require --rationale. The actor, resolution, and rationale are recorded in\n" +
	"the run journal.\n" +
	"GOOBERS_API_TOKEN supplies a bearer token when API auth is enabled, and\n" +
	"--api (or $GOOBERS_DAEMON_API) reaches a daemon that does not share this\n" +
	"filesystem, such as one running in another pod.\n\n" +
	"Exit codes: 0 = resolution accepted, 1 = resolution refused, 2 = usage/transport error.\n"

func runEscalationResolve(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("escalations resolve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "escalations resolve")
	resolution := fs.String("resolution", "", "escalation resolution: approve, deny, or redirect")
	gate := fs.String("gate", "", "escalated gate for approve and redirect")
	decision := fs.String("decision", "", "configured branch decision for redirect")
	rationale := fs.String("rationale", "", "audit rationale (required for deny and redirect)")
	actor := fs.String("actor", "", "audit identity for tier-1 local trust")
	api := fs.String("api", "", "daemon API base URL for a remote daemon (default $GOOBERS_DAEMON_API)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fs.Usage()
		return 2
	}
	input := httpapi.EscalationResolutionRequest{
		RunID:      fs.Arg(0),
		Resolution: strings.TrimSpace(*resolution),
		Gate:       strings.TrimSpace(*gate),
		Decision:   strings.TrimSpace(*decision),
		Rationale:  *rationale,
		Actor:      strings.TrimSpace(*actor),
	}
	root := "."
	if fs.NArg() == 2 {
		root = fs.Arg(1)
	}
	if code := validateEscalationResolution(input, stderr); code != 0 {
		return code
	}
	if input.Actor == "" {
		resolved, err := defaultInterventionActor()
		if err != nil {
			pf(stderr, "error: resolve intervention actor: %v\n", err)
			return 2
		}
		input.Actor = resolved
	}

	endpoint, err := remoteDaemonAPIBase(*api)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	var result httpapi.InterventionResult
	apiErr, err := callDaemonMutationAPI(
		instance.NewLayout(root), endpoint, apicontract.RouteResolveEscalation,
		map[string]string{"{run}": input.RunID}, input, &result,
	)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if apiErr != nil {
		pf(stderr, "error: %s: %s\n", apiErr.Code, apiErr.Message)
		return 1
	}
	pf(stdout, "escalation %s accepted for run %s; phase=%s", input.Resolution, input.RunID, result.Phase)
	if result.State != "" {
		pf(stdout, " state=%s", result.State)
	}
	pln(stdout, "")
	return 0
}

// validateEscalationResolution refuses locally what the plane would refuse
// anyway, so an operator gets the usage exit code and the flag name rather
// than a round trip and a typed API refusal.
func validateEscalationResolution(input httpapi.EscalationResolutionRequest, stderr io.Writer) int {
	switch input.Resolution {
	case httpapi.EscalationResolutionApprove, httpapi.EscalationResolutionRedirect:
		if input.Gate == "" {
			pf(stderr, "error: --gate is required for %s\n", input.Resolution)
			return 2
		}
	case httpapi.EscalationResolutionDeny:
	case "":
		pf(stderr, "error: --resolution is required\n")
		return 2
	default:
		pf(stderr, "error: --resolution must be approve, deny, or redirect\n")
		return 2
	}
	if input.Resolution == httpapi.EscalationResolutionRedirect && input.Decision == "" {
		pf(stderr, "error: --decision is required for redirect\n")
		return 2
	}
	if input.Resolution != httpapi.EscalationResolutionApprove && strings.TrimSpace(input.Rationale) == "" {
		pf(stderr, "error: --rationale is required for %s\n", input.Resolution)
		return 2
	}
	return 0
}
