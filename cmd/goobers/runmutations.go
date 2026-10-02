package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/instance"
)

const maxInterventionResponseBody = 1 << 20

const approveHelp = "Usage: goobers approve [--decision=pass] [--actor=<identity>] [--api=<url>] <run-id> <gate> [path]\n\n" +
	"Approve a paused human gate or an escalated human/reviewer gate. The daemon\n" +
	"records the authenticated actor, decision, and resulting resume in the run\n" +
	"journal.\n" +
	"GOOBERS_API_TOKEN supplies a bearer token when API auth is enabled, and\n" +
	"--api (or $GOOBERS_DAEMON_API) reaches a daemon that does not share this\n" +
	"filesystem, such as one running in another pod.\n\n" +
	"Exit codes: 0 = action accepted, 1 = action refused, 2 = usage/transport error.\n"

func runApprove(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("approve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "approve")
	decision := fs.String("decision", "pass", "configured gate decision to approve")
	actor := fs.String("actor", "", "audit identity for tier-1 local trust")
	api := fs.String("api", "", "daemon API base URL for a remote daemon (default $GOOBERS_DAEMON_API)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return runInterventionCLI(
		fs, apicontract.RouteApproveStage, "approve", *api,
		httpapi.InterventionRequest{Actor: *actor, Decision: *decision},
		stdout, stderr,
	)
}

const overrideHelp = "Usage: goobers override --rationale=<text> [--decision=pass] [--actor=<identity>] [--api=<url>] <run-id> <gate> [path]\n\n" +
	"Override a nondeterministic gate on an escalated or failed run and continue\n" +
	"from the selected configured branch. The rationale and authenticated actor\n" +
	"are recorded in the run journal.\n" +
	"GOOBERS_API_TOKEN supplies a bearer token when API auth is enabled, and\n" +
	"--api (or $GOOBERS_DAEMON_API) reaches a daemon that does not share this\n" +
	"filesystem, such as one running in another pod.\n\n" +
	"Exit codes: 0 = action accepted, 1 = action refused, 2 = usage/transport error.\n"

func runOverride(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("override", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "override")
	decision := fs.String("decision", "pass", "configured gate decision to force")
	rationale := fs.String("rationale", "", "required audit rationale")
	actor := fs.String("actor", "", "audit identity for tier-1 local trust")
	api := fs.String("api", "", "daemon API base URL for a remote daemon (default $GOOBERS_DAEMON_API)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*rationale) == "" {
		pf(stderr, "error: --rationale is required\n")
		fs.Usage()
		return 2
	}
	return runInterventionCLI(
		fs, apicontract.RouteOverrideStage, "override", *api,
		httpapi.InterventionRequest{Actor: *actor, Decision: *decision, Rationale: *rationale},
		stdout, stderr,
	)
}

const rerunStageHelp = "Usage: goobers rerun-stage --addendum=<text> [--actor=<identity>] [--api=<url>] <run-id> <stage> [path]\n\n" +
	"Rerun one agentic task or reviewer gate on an escalated run with a one-off\n" +
	"instruction addendum. The actor, addendum, target, and human attempt are\n" +
	"recorded in the run journal; the workflow definition is not changed.\n" +
	"GOOBERS_API_TOKEN supplies a bearer token when API auth is enabled, and\n" +
	"--api (or $GOOBERS_DAEMON_API) reaches a daemon that does not share this\n" +
	"filesystem, such as one running in another pod.\n\n" +
	"Exit codes: 0 = action accepted, 1 = action refused, 2 = usage/transport error.\n"

func runRerunStage(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("rerun-stage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "rerun-stage")
	addendum := fs.String("addendum", "", "required one-off instruction addendum")
	actor := fs.String("actor", "", "audit identity for tier-1 local trust")
	api := fs.String("api", "", "daemon API base URL for a remote daemon (default $GOOBERS_DAEMON_API)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*addendum) == "" {
		pf(stderr, "error: --addendum is required\n")
		fs.Usage()
		return 2
	}
	return runInterventionCLI(
		fs, apicontract.RouteRerunStage, "rerun-stage", *api,
		httpapi.InterventionRequest{Actor: *actor, InstructionAddendum: *addendum},
		stdout, stderr,
	)
}

func runInterventionCLI(
	fs *flag.FlagSet,
	routeID apicontract.RouteID,
	action string,
	api string,
	input httpapi.InterventionRequest,
	stdout, stderr io.Writer,
) int {
	if fs.NArg() < 2 || fs.NArg() > 3 {
		fs.Usage()
		return 2
	}
	input.RunID = fs.Arg(0)
	input.Stage = fs.Arg(1)
	root := "."
	if fs.NArg() == 3 {
		root = fs.Arg(2)
	}
	if strings.TrimSpace(input.Actor) == "" {
		actor, err := defaultInterventionActor()
		if err != nil {
			pf(stderr, "error: resolve intervention actor: %v\n", err)
			return 2
		}
		input.Actor = actor
	}

	endpoint, err := remoteDaemonAPIBase(api)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	layout := instance.NewLayout(root)
	if err := prepareLocalManualRoot(layout, endpoint, stderr); err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	result, apiErr, err := callInterventionAPI(layout, endpoint, routeID, input)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if apiErr != nil {
		pf(stderr, "error: %s: %s\n", apiErr.Code, apiErr.Message)
		return 1
	}
	pf(stdout, "%s accepted for run %s; phase=%s", action, input.RunID, result.Phase)
	if result.State != "" {
		pf(stdout, " state=%s", result.State)
	}
	pln(stdout, "")
	return 0
}

// callInterventionAPI calls one intervention route. An empty endpoint resolves
// the daemon from this instance root, which requires the daemon to share this
// filesystem; a configured endpoint names a daemon that does not (#3279), so
// resolving an escalation no longer requires being inside the daemon's pod.
func callInterventionAPI(
	layout instance.Layout,
	endpoint string,
	routeID apicontract.RouteID,
	input httpapi.InterventionRequest,
) (httpapi.InterventionResult, *apicontract.APIError, error) {
	result, apiErr, err := callDaemonMutationAPI[httpapi.InterventionRequest, httpapi.InterventionResult](
		layout, endpoint, routeID, map[string]string{
			"{run}":   input.RunID,
			"{stage}": input.Stage,
		}, input)
	if err != nil || apiErr != nil {
		return httpapi.InterventionResult{}, apiErr, err
	}
	return result, nil, nil
}

// callDaemonMutationAPI POSTs one contract mutation route and decodes its
// result. An empty endpoint resolves the daemon from this instance root, which
// requires the daemon to share this filesystem; a configured endpoint names a
// daemon that does not (#3279/#3807). pathValues substitutes the route path's
// placeholders, each escaped for a path segment.
func callDaemonMutationAPI[Req, Resp any](
	layout instance.Layout,
	endpoint string,
	routeID apicontract.RouteID,
	pathValues map[string]string,
	input Req,
) (Resp, *apicontract.APIError, error) {
	key, err := newInterventionIdempotencyKey()
	if err != nil {
		var zero Resp
		return zero, nil, fmt.Errorf("generate intervention idempotency key: %w", err)
	}
	return callDaemonMutationAPIWithKey[Req, Resp](layout, endpoint, routeID, pathValues, input, key)
}

func callDaemonMutationAPIWithKey[Req, Resp any](
	layout instance.Layout,
	endpoint string,
	routeID apicontract.RouteID,
	pathValues map[string]string,
	input Req,
	key string,
) (Resp, *apicontract.APIError, error) {
	var zero Resp
	baseURL := endpoint
	if baseURL == "" {
		config, err := instance.LoadConfig(layout.ConfigFile())
		if err != nil {
			return zero, nil, fmt.Errorf("load instance config: %w", err)
		}
		address, err := dashboardDaemonAPIAddress(layout, apiListenAddress(config))
		if err != nil {
			return zero, nil, fmt.Errorf("resolve live daemon API: %w", err)
		}
		baseURL = daemonAPIScheme(config) + "://" + address
	}
	return callDaemonJSON[Req, Resp](daemonJSONCall[Req]{
		Context:         context.Background(),
		Endpoint:        baseURL,
		RouteID:         routeID,
		PathValues:      pathValues,
		IdempotencyKey:  key,
		Input:           input,
		MaxResponseBody: maxInterventionResponseBody,
		ClientTimeout:   remoteTriggerTimeout,
		EncodePrefix:    fmt.Sprintf("encode %s request", routeID),
		BuildPrefix:     fmt.Sprintf("build %s request", routeID),
		CallPrefix:      "call live daemon API",
		DecodePrefix:    fmt.Sprintf("decode daemon %s result", routeID),
	})
}

func newInterventionIdempotencyKey() (string, error) {
	var key [16]byte
	if _, err := rand.Read(key[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", key[:]), nil
}

func defaultInterventionActor() (string, error) {
	for _, name := range []string{"GITHUB_ACTOR", "USER", "USERNAME"} {
		if actor := strings.TrimSpace(os.Getenv(name)); actor != "" {
			return actor, nil
		}
	}
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	if actor := strings.TrimSpace(current.Username); actor != "" {
		return actor, nil
	}
	return "", errors.New("current user has no username; pass --actor")
}
