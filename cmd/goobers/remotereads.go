package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/apicontract"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/readservice"
	"github.com/goobers/goobers/internal/signals"
	"github.com/goobers/goobers/internal/telemetry/rollup"
)

const maxRemoteReadBody = 32 << 20

// remoteRuns adapts the daemon's versioned read API to the same boundary used
// by the filesystem-backed diagnostic commands. Keeping the adapter behind
// OfflineRuns means rendering and filtering do not fork between local and
// remote operation.
type remoteRuns struct {
	endpoint   string
	client     *http.Client
	instanceID string
}

func newRemoteRuns(endpoint string) *remoteRuns {
	return &remoteRuns{endpoint: endpoint, client: &http.Client{
		Timeout:       remoteTriggerTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (r *remoteRuns) request(ctx context.Context, routeID apicontract.RouteID, values map[string]string, query url.Values) (*http.Response, error) {
	route, ok := apicontract.V1Route(routeID)
	if !ok {
		return nil, fmt.Errorf("API route %q is not registered", routeID)
	}
	path := route.Path
	for placeholder, value := range values {
		path = strings.ReplaceAll(path, placeholder, url.PathEscape(value))
	}
	if len(query) != 0 {
		path += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, route.Method, r.endpoint+path, nil)
	if err != nil {
		return nil, fmt.Errorf("build daemon %s request", routeID)
	}
	req.Header.Set("Accept", "application/json")
	if token := strings.TrimSpace(os.Getenv("GOOBERS_API_TOKEN")); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call daemon %s API", routeID)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer func() { _ = resp.Body.Close() }()
	var envelope apicontract.ErrorEnvelope
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRemoteTriggerResponseBody)).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("daemon %s API returned HTTP %d", routeID, resp.StatusCode)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: %s", readservice.ErrNotFound, envelope.Error.Message)
	}
	return nil, fmt.Errorf("daemon %s API: %s", routeID, envelope.Error.Message)
}

func (r *remoteRuns) json(ctx context.Context, routeID apicontract.RouteID, values map[string]string, query url.Values, out any) error {
	resp, err := r.request(ctx, routeID, values, query)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteReadBody+1))
	if err != nil || len(body) > maxRemoteReadBody {
		return fmt.Errorf("daemon %s response unreadable or oversized", routeID)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode daemon %s response: %w", routeID, err)
	}
	return nil
}

func (r *remoteRuns) ListRuns(ctx context.Context, options readservice.RunListOptions) (readservice.RunList, error) {
	q := make(url.Values)
	set := func(key, value string) {
		if value != "" {
			q.Set(key, value)
		}
	}
	set("gaggle", options.Gaggle)
	set("workflow", options.Workflow)
	set("stage", options.Stage)
	set("outcome", string(options.Outcome))
	set("population", string(options.StagePopulation))
	set("phase", string(options.Phase))
	set("trigger", string(options.Trigger))
	set("cursor", options.Cursor)
	if !options.Since.IsZero() {
		q.Set("since", options.Since.Format("2006-01-02T15:04:05.999999999Z07:00"))
	}
	if !options.Until.IsZero() {
		q.Set("until", options.Until.Format("2006-01-02T15:04:05.999999999Z07:00"))
	}
	if options.Limit != 0 {
		q.Set("limit", strconv.Itoa(options.Limit))
	}
	if options.LatestPerWorkflow {
		q.Set("latestPerWorkflow", "true")
	}
	if options.ShowNoWork {
		q.Set("showNoWork", "true")
	}
	if options.OrderByActivity {
		q.Set("orderByActivity", "true")
	}
	var result readservice.RunList
	return result, r.json(ctx, apicontract.RouteRuns, nil, q, &result)
}

func (r *remoteRuns) RunIDs(ctx context.Context) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		page, err := r.ListRuns(ctx, readservice.RunListOptions{Limit: 200, Cursor: cursor, ShowNoWork: true})
		if err != nil {
			return nil, err
		}
		for _, run := range page.Runs {
			ids = append(ids, run.ID)
		}
		if page.NextCursor == "" {
			return ids, nil
		}
		cursor = page.NextCursor
	}
}

func (r *remoteRuns) GetRun(ctx context.Context, id string) (readservice.RunDetail, error) {
	var result readservice.RunDetail
	return result, r.json(ctx, apicontract.RouteRunDetail, map[string]string{"{run}": id}, nil, &result)
}

func (r *remoteRuns) RunMetadata(ctx context.Context, id string) (journal.RunIdentity, *journal.State, error) {
	detail, err := r.GetRun(ctx, id)
	if err != nil {
		return journal.RunIdentity{}, nil, err
	}
	identity := journal.RunIdentity{InstanceID: r.instanceID, RunID: detail.ID, Workflow: detail.Workflow, WorkflowVersion: detail.WorkflowVersion, WorkflowDigest: detail.WorkflowDigest, Gaggle: detail.Gaggle, Trigger: detail.Trigger}
	state := &journal.State{RunID: detail.ID, Phase: detail.Phase, MachineState: detail.CurrentStage, LastSeq: detail.LastSeq, UpdatedAt: detail.LastActivityAt}
	return identity, state, nil
}

func (r *remoteRuns) RunEvents(ctx context.Context, id string) (readservice.EventList, error) {
	var result readservice.EventList
	return result, r.json(ctx, apicontract.RouteRunEvents, map[string]string{"{run}": id}, nil, &result)
}

func (r *remoteRuns) StageAttempts(ctx context.Context, id, stage string) (readservice.AttemptList, error) {
	var result readservice.AttemptList
	return result, r.json(ctx, apicontract.RouteStageAttempts, map[string]string{"{run}": id, "{stage}": stage}, nil, &result)
}

func (r *remoteRuns) Artifact(ctx context.Context, id, digest string) (readservice.ArtifactContent, error) {
	resp, err := r.request(ctx, apicontract.RouteRunArtifact, map[string]string{"{run}": id, "{digest}": digest}, nil)
	if err != nil {
		return readservice.ArtifactContent{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteReadBody+1))
	if err != nil || len(data) > maxRemoteReadBody {
		return readservice.ArtifactContent{}, errors.New("daemon artifact response unreadable or oversized")
	}
	return readservice.ArtifactContent{Metadata: readservice.ArtifactMetadata{Digest: resp.Header.Get(apicontract.DigestHeader), Size: int64(len(data)), MediaType: resp.Header.Get("Content-Type")}, Bytes: data}, nil
}

func (r *remoteRuns) Transcript(ctx context.Context, id string, seq uint64) (readservice.TranscriptContent, error) {
	resp, err := r.request(ctx, apicontract.RouteRunTranscript, map[string]string{"{run}": id, "{seq}": strconv.FormatUint(seq, 10)}, nil)
	if err != nil {
		return readservice.TranscriptContent{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteReadBody+1))
	if err != nil || len(data) > maxRemoteReadBody {
		return readservice.TranscriptContent{}, errors.New("daemon transcript response unreadable or oversized")
	}
	return readservice.TranscriptContent{Seq: seq, Stage: resp.Header.Get("X-Goobers-Stage"), Name: resp.Header.Get("X-Goobers-Transcript-Name"), Bytes: data}, nil
}

func (r *remoteRuns) RunTranscripts(ctx context.Context, id, stage string) ([]readservice.TranscriptContent, error) {
	ledger, err := r.RunEvents(ctx, id)
	if err != nil {
		return nil, err
	}
	completed := make(map[string]bool)
	for _, event := range ledger.Events {
		capture, _ := event.Runner["transcriptCaptureComplete"].(string)
		if event.KnownSchema && event.Type == journal.EventSpanRecorded && capture != "" && event.Artifact != nil {
			completed[capture] = true
		}
	}
	var result []readservice.TranscriptContent
	for _, event := range ledger.Events {
		recordedStage := strings.TrimPrefix(event.Stage, id+":")
		partial := event.Runner["partial"] == true && (event.Name == "transcript.partial" || strings.HasSuffix(event.Name, ".transcript.partial"))
		capture, _ := event.Runner["transcriptCapture"].(string)
		if partial && capture != "" && completed[capture] {
			continue
		}
		if !event.KnownSchema || event.Type != journal.EventSpanRecorded || (event.Name != "transcript" && !strings.HasSuffix(event.Name, ".transcript") && !partial) || (stage != "" && recordedStage != stage) {
			continue
		}
		transcript, err := r.Transcript(ctx, id, event.Seq)
		if err != nil {
			return nil, err
		}
		result = append(result, transcript)
	}
	return result, nil
}

func (*remoteRuns) RunSpans(context.Context, string) ([]rollup.SpanSummary, error) { return nil, nil }
func (r *remoteRuns) RunTelemetryStageAttempts(ctx context.Context, id string) ([]rollup.StageAttempt, error) {
	ledger, err := r.RunEvents(ctx, id)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var result []rollup.StageAttempt
	for _, event := range ledger.Events {
		if !event.KnownSchema || event.Stage == "" || seen[event.Stage] {
			continue
		}
		seen[event.Stage] = true
		attempts, err := r.StageAttempts(ctx, id, event.Stage)
		if err != nil {
			return nil, err
		}
		for _, attempt := range attempts.Attempts {
			converted := rollup.StageAttempt{Stage: event.Stage, Traversal: attempt.Visit, Attempt: attempt.Number, Model: attempt.Model, AttemptClass: attempt.Class, Status: attempt.Status, DurationMs: attempt.DurationMillis, ErrorCode: attempt.ErrorCode, ErrorClass: attempt.ErrorClass}
			if attempt.StartedAt != nil {
				converted.StartedAt = *attempt.StartedAt
			}
			if attempt.FinishedAt != nil {
				converted.FinishedAt = *attempt.FinishedAt
			}
			result = append(result, converted)
		}
	}
	return result, nil
}
func (r *remoteRuns) RunEscalation(ctx context.Context, id string) (*readservice.TraceEscalation, error) {
	detail, err := r.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	if detail.Escalation == nil {
		return nil, nil
	}
	return &readservice.TraceEscalation{RepassCount: detail.Escalation.RepassCount}, nil
}
func (r *remoteRuns) RunTraceRepassCount(ctx context.Context, id string) (int, error) {
	ledger, err := r.RunEvents(ctx, id)
	if err != nil {
		return 0, err
	}
	legacy := 0
	for _, event := range ledger.Events {
		if event.KnownSchema && event.Type == journal.EventGateEvaluated && event.Target == "implement" {
			legacy++
		}
	}
	if legacy > 0 {
		return legacy, nil
	}
	detail, err := r.GetRun(ctx, id)
	return detail.RepassCount, err
}

func prepareRemoteReads(ctx context.Context, endpoint, root string, rootExplicit bool, diagnostic io.Writer) (*remoteRuns, error) {
	expectedID := ""
	if rootExplicit {
		var err error
		expectedID, err = instance.ReadRootIdentity(root)
		if err != nil {
			return nil, fmt.Errorf("read expected instance identity: %w", err)
		}
	}
	if err := prepareRemoteRootForInstance(ctx, endpoint, expectedID, diagnostic); err != nil {
		return nil, err
	}
	reads := newRemoteRuns(endpoint)
	remote, err := reads.Instance(ctx)
	if err != nil {
		return nil, err
	}
	if remote.RootIdentity != nil {
		reads.instanceID = remote.RootIdentity.ID
	}
	return reads, nil
}

func resolveRemoteRunID(ctx context.Context, reads readservice.OfflineRuns, arg string) (string, error) {
	ids, err := reads.RunIDs(ctx)
	if err != nil {
		return "", err
	}
	var matches []string
	for _, id := range ids {
		if id == arg {
			return id, nil
		}
		if strings.HasPrefix(id, arg) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("run %q: %w", arg, iofs.ErrNotExist)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous prefix %q matches %d runs: %s", arg, len(matches), strings.Join(matches, ", "))
	}
}

func (r *remoteRuns) Instance(ctx context.Context) (readservice.Instance, error) {
	var result readservice.Instance
	return result, r.json(ctx, apicontract.RouteInstance, nil, nil, &result)
}

func remoteStatusRuns(ctx context.Context, reads *remoteRuns) ([]runSummary, error) {
	var runs []runSummary
	cursor := ""
	for {
		page, err := reads.ListRuns(ctx, readservice.RunListOptions{Limit: 200, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, run := range page.Runs {
			runs = append(runs, runSummary{EngineFallback: run.EngineFallback, RunID: run.ID, Workflow: run.Workflow, Gaggle: run.Gaggle, Phase: run.Phase, StartedAt: run.StartedAt, LastActivityAt: run.LastActivityAt, Operator: run.Operator})
		}
		if page.NextCursor == "" {
			return runs, nil
		}
		cursor = page.NextCursor
	}
}

func runRemoteRunTable(ctx context.Context, endpoint, root string, rootExplicit, jsonOutput, watch bool, interval time.Duration, options statusOptions, stdout, stderr io.Writer) int {
	reads, err := prepareRemoteReads(ctx, endpoint, root, rootExplicit, stderr)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	remoteInstance, err := reads.Instance(ctx)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	loadRuns := func() ([]runSummary, error) { return remoteStatusRuns(ctx, reads) }
	if watch {
		followCtx, stop := signals.SetupSignalContext()
		defer stop()
		loadStatus := func(context.Context, []runSummary, time.Time) (string, error) {
			return remoteStatusText(remoteInstance), nil
		}
		if err := watchStatus(followCtx, interval, options, stdout, loadRuns, loadStatus); err != nil {
			pf(stderr, "error: %v\n", err)
			return 2
		}
		return 0
	}
	runs, err := loadRuns()
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	runs, older := selectStatusRuns(runs, options)
	if jsonOutput {
		output := statusJSONOutput{Root: remoteStatusRoot(remoteInstance), Warnings: remoteInstance.Warnings, Maintenance: remoteInstance.Maintenance, Runs: statusJSONSummaries(runs)}
		if err := json.NewEncoder(stdout).Encode(output); err != nil {
			pf(stderr, "error: encode status: %v\n", err)
			return 2
		}
		return 0
	}
	printValidationWarnings(stdout, remoteInstance.Warnings)
	pf(stdout, "%s", remoteStatusText(remoteInstance))
	renderStatus(stdout, runs, time.Now())
	renderOlderRunsHint(stdout, older)
	return 0
}

func maybeRunRemoteRunTable(api, root string, rootExplicit, supportsWatch bool, daemon, agents, watch *bool, interval *time.Duration, jsonOutput bool, options statusOptions, stdout, stderr io.Writer) (int, bool) {
	endpoint, err := remoteDaemonAPIBase(api)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2, true
	}
	if endpoint == "" {
		return 0, false
	}
	if supportsWatch && (*daemon || *agents) {
		pf(stderr, "error: --api cannot be combined with --daemon or --agents\n")
		return 2, true
	}
	remoteWatch, remoteInterval := false, defaultStatusWatchInterval
	if supportsWatch {
		remoteWatch, remoteInterval = *watch, *interval
	}
	return runRemoteRunTable(context.Background(), endpoint, root, rootExplicit, jsonOutput, remoteWatch, remoteInterval, options, stdout, stderr), true
}

func remoteStatusRoot(remote readservice.Instance) *statusRootIdentity {
	root := &statusRootIdentity{Path: remote.InstanceRoot, DaemonState: "healthy"}
	if !remote.Ready {
		root.DaemonState = "unhealthy"
	}
	if remote.RootIdentity != nil {
		root.ID = remote.RootIdentity.ID
		root.IdentityProblem = remote.RootIdentity.IdentityProblem
		root.DecommissionedAt = remote.RootIdentity.DecommissionedAt
		root.DecommissionReason = remote.RootIdentity.DecommissionReason
		root.LifecycleProblem = remote.RootIdentity.LifecycleProblem
	}
	return root
}

func remoteStatusText(remote readservice.Instance) string {
	var out strings.Builder
	writeStatusRoot(&out, *remoteStatusRoot(remote))
	if remote.Maintenance != nil {
		out.WriteString(maintenanceStatusLine(readservice.SchedulerStatus{Maintenance: remote.Maintenance}))
	}
	return out.String()
}
