// Package operatorcli implements the API-backed goobers-cloud operator surface.
package operatorcli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/daemonclient"
	"github.com/goobers/goobers/internal/httpapi"
)

// Command is one registry entry consumed by cmd/goobers-cloud and docs generation.
type Command struct {
	Name  string
	Short string
	Long  string
	Run   func([]string, io.Writer, io.Writer) int
}

// Handler is a command implementation supplied by the composition root.
type Handler func([]string, io.Writer, io.Writer) int

// Commands returns the complete operator command registry.
func Commands(overrides ...map[string]Handler) []Command {
	commands := []Command{
		{Name: "status", Short: "Show cloud instance status through the API.", Long: statusHelp, Run: runStatus},
		{Name: "runs list", Short: "List runs through the API.", Long: runsListHelp, Run: runRunsList},
		{Name: "runs show", Short: "Show one run through the API.", Long: runsShowHelp, Run: runRunsShow},
		{Name: "runs logs", Short: "Read one run's journal events through the API.", Long: runsLogsHelp, Run: runRunsLogs},
		{Name: "claims list", Short: "List active claims through the API.", Long: claimsListHelp, Run: runClaimsList},
		{Name: "claims release", Short: "Release a cloud claim.", Long: claimsReleaseHelp, Run: unavailableOperatorCommand("claims release", claimsReleaseHelp, "#7808 (implementation PR #7845)")},
		{Name: "triggers", Short: "Submit a durable workflow trigger through the API.", Long: triggersHelp, Run: runTriggers},
		{Name: "approve", Short: "Approve a paused stage through the API.", Long: approveHelp, Run: runApprove},
		{Name: "override", Short: "Override a paused stage through the API.", Long: overrideHelp, Run: runOverride},
		{Name: "deny", Short: "Deny an escalated run through the API.", Long: denyHelp, Run: runDeny},
		{Name: "cancel", Short: "Cancel a live run through the API.", Long: cancelHelp, Run: runCancel},
		{Name: "config apply", Short: "Apply cloud configuration.", Long: configApplyHelp, Run: unavailableOperatorCommand("config apply", configApplyHelp, "#7834")},
		{Name: "config status", Short: "Show cloud configuration status.", Long: configStatusHelp, Run: unavailableOperatorCommand("config status", configStatusHelp, "#7834")},
		{Name: "migrate status", Short: "Show cloud schema migration status.", Long: migrateStatusHelp},
		{Name: "lease status", Short: "Show cloud controller lease status.", Long: leaseStatusHelp},
	}
	if len(overrides) != 0 {
		for i := range commands {
			if run := overrides[0][commands[i].Name]; run != nil {
				commands[i].Run = run
			}
		}
	}
	for i := range commands {
		if commands[i].Run == nil {
			commands[i].Run = unavailableOperatorCommand(commands[i].Name, commands[i].Long, "#7818")
		}
	}
	return commands
}

// Help returns the registered long help for name.
func Help(name string) string {
	for _, command := range Commands() {
		if command.Name == name {
			return command.Long
		}
	}
	return ""
}

const (
	statusHelp = `Usage: goobers-cloud status --api URL

Show the cloud instance identity and status through the control-plane API.
`
	runsListHelp = `Usage: goobers-cloud runs list --api URL [--gaggle NAME] [--workflow NAME] [--phase PHASE] [--limit N]

List cloud runs through the control-plane API.
`
	runsShowHelp = `Usage: goobers-cloud runs show --api URL RUN

Show one cloud run through the control-plane API.
`
	runsLogsHelp = `Usage: goobers-cloud runs logs --api URL RUN

Read one cloud run's journal events through the control-plane API.
`
	claimsListHelp = `Usage: goobers-cloud claims list --api URL

List active cloud claims through the control-plane API.
`
	claimsReleaseHelp = `Usage: goobers-cloud claims release [OPTIONS]

Release a cloud claim. This command is reserved until the PostgreSQL claims
backend supplies an operator-safe release operation (#7808; implementation PR
#7845).
`
	triggersHelp = `Usage: goobers-cloud triggers --api URL --workflow NAME [--gaggle NAME] --idempotency-key KEY [--force] [--source-run RUN]

Submit one durable workflow trigger through the control-plane API.
`
	approveHelp = `Usage: goobers-cloud approve --api URL --idempotency-key KEY [--actor NAME] [--rationale TEXT] RUN STAGE

Approve one paused or escalated stage through the control-plane API.
`
	overrideHelp = `Usage: goobers-cloud override --api URL --idempotency-key KEY --decision NAME --rationale TEXT [--actor NAME] RUN STAGE

Override one paused stage through the control-plane API.
`
	denyHelp = `Usage: goobers-cloud deny --api URL --idempotency-key KEY --rationale TEXT [--actor NAME] RUN

Deny one escalated run through the control-plane API.
`
	cancelHelp = `Usage: goobers-cloud cancel --api URL --idempotency-key KEY [--actor NAME] [--gaggle NAME] [--workflow NAME] RUN

Cancel one live cloud run through the control-plane API.
`
	configApplyHelp = `Usage: goobers-cloud config apply [OPTIONS]

Apply a cloud configuration generation. Reserved until #7834.
`
	configStatusHelp = `Usage: goobers-cloud config status [OPTIONS]

Show cloud configuration generation and eligibility status. Reserved until #7834.
`
	migrateStatusHelp = `Usage: goobers-cloud migrate status [DATABASE OPTIONS]

Show applied cloud schema migrations and binary compatibility.
`
	leaseStatusHelp = `Usage: goobers-cloud lease status [DATABASE OPTIONS]

Show active cloud controller leases, fence epochs, DB-clock expiry, and recent
lease history.
`
)

var newOperatorHTTPClient = daemonclient.NewHTTP

func write(w io.Writer, text string) { _, _ = io.WriteString(w, text) }
func writef(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func unavailableOperatorCommand(name, help, issue string) func([]string, io.Writer, io.Writer) int {
	return func(args []string, stdout, stderr io.Writer) int {
		if len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			write(stdout, help)
			return 0
		}
		if len(args) != 0 {
			write(stderr, help)
			return 2
		}
		writef(stderr, "goobers-cloud %s: not available until %s\n", name, issue)
		return 1
	}
}

func runStatus(args []string, stdout, stderr io.Writer) int {
	flags, api := operatorFlagSet("status", statusHelp, stderr)
	if endpoint, _, code, ok := parseOperatorFlags(flags, api, args, 0, 0); ok {
		return callOperatorAPI(endpoint, apicontract.RouteInstance, nil, nil, "", stdout, stderr)
	} else {
		return code
	}
}

func runRunsList(args []string, stdout, stderr io.Writer) int {
	flags, api := operatorFlagSet("runs list", runsListHelp, stderr)
	gaggle := flags.String("gaggle", "", "filter by gaggle")
	workflow := flags.String("workflow", "", "filter by workflow")
	phase := flags.String("phase", "", "filter by phase")
	limit := flags.Int("limit", 0, "maximum runs to return")
	endpoint, _, code, ok := parseOperatorFlags(flags, api, args, 0, 0)
	if !ok {
		return code
	}
	query := url.Values{}
	query.Set("gaggle", strings.TrimSpace(*gaggle))
	query.Set("workflow", strings.TrimSpace(*workflow))
	query.Set("phase", strings.TrimSpace(*phase))
	if *limit > 0 {
		query.Set("limit", strconv.Itoa(*limit))
	}
	return callOperatorAPI(endpoint, apicontract.RouteRuns, nil, query, "", stdout, stderr)
}

func runRunsShow(args []string, stdout, stderr io.Writer) int {
	return runSingleRunRead("runs show", runsShowHelp, apicontract.RouteRunDetail, args, stdout, stderr)
}

func runRunsLogs(args []string, stdout, stderr io.Writer) int {
	return runSingleRunRead("runs logs", runsLogsHelp, apicontract.RouteRunEvents, args, stdout, stderr)
}

func runSingleRunRead(name, help string, route apicontract.RouteID, args []string, stdout, stderr io.Writer) int {
	flags, api := operatorFlagSet(name, help, stderr)
	endpoint, positional, code, ok := parseOperatorFlags(flags, api, args, 1, 1)
	if !ok {
		return code
	}
	return callOperatorAPI(endpoint, route, map[string]string{"{run}": positional[0]}, nil, "", stdout, stderr)
}

func runClaimsList(args []string, stdout, stderr io.Writer) int {
	flags, api := operatorFlagSet("claims list", claimsListHelp, stderr)
	endpoint, _, code, ok := parseOperatorFlags(flags, api, args, 0, 0)
	if !ok {
		return code
	}
	return callOperatorAPI(endpoint, apicontract.RouteClaimsActive, nil, nil, "", stdout, stderr)
}

func runTriggers(args []string, stdout, stderr io.Writer) int {
	flags, api := operatorFlagSet("triggers", triggersHelp, stderr)
	workflow := flags.String("workflow", "", "workflow to trigger")
	gaggle := flags.String("gaggle", "", "gaggle owning the workflow")
	key := flags.String("idempotency-key", "", "durable trigger request identity")
	sourceRun := flags.String("source-run", "", "source run for a priority trigger")
	force := flags.Bool("force", false, "bypass hourly and daily cadence budgets")
	endpoint, _, code, ok := parseOperatorFlags(flags, api, args, 0, 0)
	if !ok {
		return code
	}
	if strings.TrimSpace(*workflow) == "" || strings.TrimSpace(*key) == "" {
		write(stderr, triggersHelp)
		return 2
	}
	input := apicontract.TriggerRequest{
		Workflow: strings.TrimSpace(*workflow), Gaggle: strings.TrimSpace(*gaggle),
		RequestID: strings.TrimSpace(*key), SourceRun: strings.TrimSpace(*sourceRun), Force: *force,
	}
	return callOperatorAPI(endpoint, apicontract.RouteTriggerIngest, nil, nil, *key, stdout, stderr, input)
}

func runApprove(args []string, stdout, stderr io.Writer) int {
	return runIntervention("approve", approveHelp, apicontract.RouteApproveStage, args, stdout, stderr)
}

func runOverride(args []string, stdout, stderr io.Writer) int {
	return runIntervention("override", overrideHelp, apicontract.RouteOverrideStage, args, stdout, stderr)
}

func runIntervention(name, help string, route apicontract.RouteID, args []string, stdout, stderr io.Writer) int {
	flags, api := operatorFlagSet(name, help, stderr)
	key := flags.String("idempotency-key", "", "durable mutation identity")
	actor := flags.String("actor", "", "audit actor")
	decision := flags.String("decision", "", "override decision")
	rationale := flags.String("rationale", "", "audit rationale")
	addendum := flags.String("instruction-addendum", "", "instruction addendum")
	endpoint, positional, code, ok := parseOperatorFlags(flags, api, args, 2, 2)
	if !ok {
		return code
	}
	if strings.TrimSpace(*key) == "" || (name == "override" && (strings.TrimSpace(*decision) == "" || strings.TrimSpace(*rationale) == "")) {
		write(stderr, help)
		return 2
	}
	input := httpapi.InterventionRequest{
		Actor: strings.TrimSpace(*actor), Decision: strings.TrimSpace(*decision),
		Rationale: *rationale, InstructionAddendum: *addendum,
	}
	return callOperatorAPI(endpoint, route, map[string]string{"{run}": positional[0], "{stage}": positional[1]}, nil, *key, stdout, stderr, input)
}

func runDeny(args []string, stdout, stderr io.Writer) int {
	flags, api := operatorFlagSet("deny", denyHelp, stderr)
	key := flags.String("idempotency-key", "", "durable mutation identity")
	actor := flags.String("actor", "", "audit actor")
	rationale := flags.String("rationale", "", "audit rationale")
	endpoint, positional, code, ok := parseOperatorFlags(flags, api, args, 1, 1)
	if !ok {
		return code
	}
	if strings.TrimSpace(*key) == "" || strings.TrimSpace(*rationale) == "" {
		write(stderr, denyHelp)
		return 2
	}
	input := httpapi.EscalationResolutionRequest{
		Actor: strings.TrimSpace(*actor), Resolution: httpapi.EscalationResolutionDeny, Rationale: *rationale,
	}
	return callOperatorAPI(endpoint, apicontract.RouteResolveEscalation, map[string]string{"{run}": positional[0]}, nil, *key, stdout, stderr, input)
}

func runCancel(args []string, stdout, stderr io.Writer) int {
	flags, api := operatorFlagSet("cancel", cancelHelp, stderr)
	key := flags.String("idempotency-key", "", "durable mutation identity")
	actor := flags.String("actor", "", "audit actor")
	gaggle := flags.String("gaggle", "", "run gaggle")
	workflow := flags.String("workflow", "", "run workflow")
	endpoint, positional, code, ok := parseOperatorFlags(flags, api, args, 1, 1)
	if !ok {
		return code
	}
	if strings.TrimSpace(*key) == "" {
		write(stderr, cancelHelp)
		return 2
	}
	input := httpapi.CancelRunRequest{
		Actor: strings.TrimSpace(*actor), Gaggle: strings.TrimSpace(*gaggle), Workflow: strings.TrimSpace(*workflow),
	}
	return callOperatorAPI(endpoint, apicontract.RouteCancelRun, map[string]string{"{run}": positional[0]}, nil, *key, stdout, stderr, input)
}

func operatorFlagSet(name, help string, stderr io.Writer) (*flag.FlagSet, *string) {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { write(stderr, help) }
	api := flags.String("api", "", "cloud control-plane API URL (default $GOOBERS_CLOUD_API)")
	return flags, api
}

func parseOperatorFlags(flags *flag.FlagSet, api *string, args []string, minArgs, maxArgs int) (string, []string, int, bool) {
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", nil, 0, false
		}
		return "", nil, 2, false
	}
	if flags.NArg() < minArgs || flags.NArg() > maxArgs {
		flags.Usage()
		return "", nil, 2, false
	}
	endpoint, err := operatorAPIBase(*api)
	if err != nil {
		writef(flags.Output(), "goobers-cloud %s: %v\n", flags.Name(), err)
		return "", nil, 2, false
	}
	return endpoint, flags.Args(), 0, true
}

func operatorAPIBase(explicit string) (string, error) {
	value := strings.TrimSpace(explicit)
	if value == "" {
		value = strings.TrimSpace(os.Getenv("GOOBERS_CLOUD_API"))
	}
	if value == "" {
		return "", errors.New("--api or GOOBERS_CLOUD_API is required")
	}
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("--api must be an absolute http(s) URL without credentials, a query, or a fragment")
	}
	return strings.TrimRight(value, "/"), nil
}

func callOperatorAPI(endpoint string, routeID apicontract.RouteID, pathValues map[string]string, query url.Values, idempotencyKey string, stdout, stderr io.Writer, body ...any) int {
	route, ok := apicontract.V1Route(routeID)
	if !ok {
		writef(stderr, "goobers-cloud: API route %s is not registered\n", routeID)
		return 2
	}
	path := route.Path
	for placeholder, value := range pathValues {
		path = strings.ReplaceAll(path, placeholder, url.PathEscape(value))
	}
	target := endpoint + path
	if len(query) != 0 {
		for key, values := range query {
			if len(values) == 1 && values[0] == "" {
				query.Del(key)
			}
		}
		if encoded := query.Encode(); encoded != "" {
			target += "?" + encoded
		}
	}
	var content io.Reader
	if len(body) != 0 {
		encoded, err := json.Marshal(body[0])
		if err != nil {
			writef(stderr, "goobers-cloud: encode request: %v\n", err)
			return 2
		}
		content = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(route.Method, target, content)
	if err != nil {
		writef(stderr, "goobers-cloud: build request: %v\n", err)
		return 2
	}
	request.Header.Set("Accept", "application/json")
	if content != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", strings.TrimSpace(idempotencyKey))
	}
	if token := strings.TrimSpace(os.Getenv("GOOBERS_API_TOKEN")); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := newOperatorHTTPClient(30 * time.Second).Do(request)
	if err != nil {
		writef(stderr, "goobers-cloud: API request failed: %v\n", err)
		return 2
	}
	defer func() { _ = response.Body.Close() }()
	const maxResponseBytes = 4 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		writef(stderr, "goobers-cloud: read API response: %v\n", err)
		return 2
	}
	if len(data) > maxResponseBytes {
		writef(stderr, "goobers-cloud: API response exceeds %d bytes\n", maxResponseBytes)
		return 2
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope apicontract.ErrorEnvelope
		if json.Unmarshal(data, &envelope) == nil && envelope.Error.Code != "" {
			writef(stderr, "goobers-cloud: %s: %s\n", envelope.Error.Code, envelope.Error.Message)
		} else {
			writef(stderr, "goobers-cloud: API returned %s\n", response.Status)
		}
		return 1
	}
	if len(data) != 0 {
		_, _ = stdout.Write(data)
		if data[len(data)-1] != '\n' {
			write(stdout, "\n")
		}
	}
	return 0
}
