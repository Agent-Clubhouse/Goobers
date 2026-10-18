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

const (
	maxRemoteReadBody                = 32 << 20
	maxRemoteStatusClientFilterPages = 5
)

// remoteRuns adapts the daemon's versioned read API to the same boundary used
// by the filesystem-backed diagnostic commands. Keeping the adapter behind
// OfflineRuns means rendering and filtering do not fork between local and
// remote operation.
type remoteRuns struct {
	endpoint string
	client   *http.Client
	// instanceID and instanceRoot are the identity prepareRemoteReads
	// validated. confirmIdentity requires every later snapshot to match both,
	// so the instance that was checked is the instance that is rendered.
	instanceID   string
	instanceRoot string
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
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, maxRemoteTriggerResponseBody)).Decode(&envelope)
	// A 404 means not found whether or not the body is the daemon's error
	// envelope (a proxy in front of it may answer plainly); callers such as
	// run-ID resolution rely on telling "absent" apart from a failed read.
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: daemon %s API: %s", readservice.ErrNotFound, routeID, envelope.Error.Message)
	}
	if decodeErr != nil {
		return nil, fmt.Errorf("daemon %s API returned HTTP %d", routeID, resp.StatusCode)
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
	// Only fields the run detail actually carries are set. The journal's
	// machine cursor, branches and the rest of the run identity are not served
	// over the API, and CurrentStage is a display label rather than the machine
	// state, so they stay empty instead of being approximated.
	identity := journal.RunIdentity{InstanceID: r.instanceID, RunID: detail.ID, Workflow: detail.Workflow, WorkflowVersion: detail.WorkflowVersion, WorkflowDigest: detail.WorkflowDigest, Gaggle: detail.Gaggle, Trigger: detail.Trigger}
	state := &journal.State{RunID: detail.ID, Phase: detail.Phase, LastSeq: detail.LastSeq, UpdatedAt: detail.LastActivityAt}
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

// Artifact reads are content-addressed, so the adapter verifies them the same
// way journalclient does: the served digest header must name the artifact that
// was asked for, and the bytes must hash to it. A daemon (or anything between
// it and the CLI) cannot substitute content under a trusted digest.
func (r *remoteRuns) Artifact(ctx context.Context, id, digest string) (readservice.ArtifactContent, error) {
	if _, err := journal.ArtifactPath(digest); err != nil {
		return readservice.ArtifactContent{}, err
	}
	resp, err := r.request(ctx, apicontract.RouteRunArtifact, map[string]string{"{run}": id, "{digest}": digest}, nil)
	if err != nil {
		return readservice.ArtifactContent{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRemoteReadBody+1))
	if err != nil || len(data) > maxRemoteReadBody {
		return readservice.ArtifactContent{}, errors.New("daemon artifact response unreadable or oversized")
	}
	if served := strings.TrimSpace(resp.Header.Get(apicontract.DigestHeader)); served != digest {
		return readservice.ArtifactContent{}, fmt.Errorf("asked the daemon for artifact %s and it served %q", digest, served)
	}
	if got := journal.Digest(data); got != digest {
		return readservice.ArtifactContent{}, fmt.Errorf("daemon artifact %s failed digest verification: have %s", digest, got)
	}
	return readservice.ArtifactContent{Metadata: readservice.ArtifactMetadata{Digest: digest, Size: int64(len(data)), MediaType: resp.Header.Get("Content-Type")}, Bytes: data}, nil
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
	// Transcripts are rendered from their span event rather than served
	// verbatim, so there is no content address to check; the daemon must at
	// least name the event it answered for.
	if served := resp.Header.Get("X-Goobers-Event-Sequence"); served != strconv.FormatUint(seq, 10) {
		return readservice.TranscriptContent{}, fmt.Errorf("asked the daemon for the transcript at seq %d and it served seq %q", seq, served)
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

// RunSpans and RunAgentProgress have no daemon read route yet. They return
// nothing rather than an error because trace treats both as enrichment; trace
// names every section missing over --api (remoteTraceOmissions) so the gap is
// stated rather than silent.
func (*remoteRuns) RunSpans(context.Context, string) ([]rollup.SpanSummary, error) { return nil, nil }
func (*remoteRuns) RunAgentProgress(context.Context, string) ([]readservice.AgentProgressSummary, error) {
	return nil, nil
}
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

// prepareRemoteReads fetches the daemon's identity once, validates that same
// snapshot (usable durable identity, live lifecycle, and the expected instance
// when a [path] pins one), and keeps it for rendering. Validating one response
// and then rendering another would let the two disagree.
func prepareRemoteReads(ctx context.Context, endpoint, root string, rootExplicit bool, diagnostic io.Writer) (*remoteRuns, error) {
	expectedID := ""
	if rootExplicit {
		var err error
		expectedID, err = instance.ReadRootIdentity(root)
		if err != nil {
			return nil, fmt.Errorf("read expected instance identity: %w", err)
		}
	}
	reads := newRemoteRuns(endpoint)
	remote, err := reads.Instance(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateRemoteRoot(remote.InstanceRoot, remote.RootIdentity); err != nil {
		return nil, err
	}
	if expectedID != "" && remote.RootIdentity.ID != expectedID {
		return nil, fmt.Errorf("daemon at --api belongs to another instance than %s; refusing the read", root)
	}
	reads.instanceID = remote.RootIdentity.ID
	reads.instanceRoot = remote.InstanceRoot
	if _, err := fmt.Fprintf(diagnostic, "Remote instance root: %q; instance ID: %q\n", remote.InstanceRoot, reads.instanceID); err != nil {
		return nil, fmt.Errorf("display remote read target: %w", err)
	}
	return reads, nil
}

// confirmIdentity re-reads the daemon's identity after the command's reads and
// refuses to render if it changed, so a daemon swapped behind the endpoint
// mid-command cannot have its answers attributed to the validated instance.
// It returns the fresh snapshot for callers that re-render it (status --watch).
func (r *remoteRuns) confirmIdentity(ctx context.Context) (readservice.Instance, error) {
	remote, err := r.Instance(ctx)
	if err != nil {
		return readservice.Instance{}, err
	}
	if err := validateRemoteRoot(remote.InstanceRoot, remote.RootIdentity); err != nil {
		return readservice.Instance{}, err
	}
	if remote.RootIdentity.ID != r.instanceID || remote.InstanceRoot != r.instanceRoot {
		return readservice.Instance{}, fmt.Errorf("daemon instance changed during the read (was %s at %q, now %s at %q); refusing to render", r.instanceID, r.instanceRoot, remote.RootIdentity.ID, remote.InstanceRoot)
	}
	return remote, nil
}

func resolveRemoteRunID(ctx context.Context, reads readservice.OfflineRuns, arg string) (string, error) {
	// A full run ID resolves with one detail read; only a prefix needs the
	// paged run list.
	if detail, err := reads.GetRun(ctx, arg); err == nil && detail.ID == arg {
		return arg, nil
	} else if err != nil && !errors.Is(err, readservice.ErrNotFound) {
		return "", err
	}
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

func remoteStatusRuns(ctx context.Context, reads *remoteRuns, options statusOptions, bounded bool) ([]runSummary, error) {
	var runs []runSummary
	cursor := ""
	pages := 0
	clientFilteredPhases := len(options.phases) > 1
	for {
		request := readservice.RunListOptions{
			Gaggle: options.gaggle, Workflow: options.workflow,
			Limit: 200, Cursor: cursor, ShowNoWork: true,
		}
		if len(options.phases) == 1 {
			for phase := range options.phases {
				request.Phase = phase
			}
		}
		if bounded && options.limit > 0 && !clientFilteredPhases {
			remaining := options.limit + 1 - len(runs)
			if remaining < request.Limit {
				request.Limit = remaining
			}
		}
		// Local status includes no-work runs (readservice status uses
		// IncludeNoWork), so the remote table asks for them too.
		page, err := reads.ListRuns(ctx, request)
		if err != nil {
			return nil, err
		}
		pages++
		for _, run := range page.Runs {
			summary := runSummary{EngineFallback: run.EngineFallback, RunID: run.ID, Workflow: run.Workflow, Gaggle: run.Gaggle, Phase: run.Phase, StartedAt: run.StartedAt, LastActivityAt: run.LastActivityAt, Operator: run.Operator, Lineage: run.Lineage}
			if statusRunMatches(summary, options) {
				runs = append(runs, summary)
			}
		}
		if page.NextCursor == "" || bounded && options.limit > 0 && len(runs) > options.limit {
			return runs, nil
		}
		if bounded && options.limit > 0 && clientFilteredPhases && pages >= maxRemoteStatusClientFilterPages {
			return nil, fmt.Errorf("remote multi-phase run scan exceeded %d pages; refine --gaggle, --workflow, or --phase filters", maxRemoteStatusClientFilterPages)
		}
		cursor = page.NextCursor
	}
}

func runRemoteRunTable(ctx context.Context, endpoint, root string, rootExplicit, jsonOutput, watch, bounded bool, interval time.Duration, options statusOptions, stdout, stderr io.Writer) int {
	reads, err := prepareRemoteReads(ctx, endpoint, root, rootExplicit, stderr)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	pf(stderr, "%s", remoteStatusOmissionNote)
	loadRuns := func() ([]runSummary, error) { return remoteStatusRuns(ctx, reads, options, bounded) }
	if watch {
		followCtx, stop := signals.SetupSignalContext()
		defer stop()
		loadStatus := func(tickCtx context.Context, _ []runSummary, _ time.Time) (string, error) {
			remote, err := reads.confirmIdentity(tickCtx)
			if err != nil {
				return "", err
			}
			return remoteStatusText(remote), nil
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
	remoteInstance, err := reads.confirmIdentity(ctx)
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

func maybeRunRemoteRunTable(api, root string, rootExplicit, supportsWatch, bounded bool, daemon, agents, watch *bool, interval *time.Duration, jsonOutput bool, options statusOptions, stdout, stderr io.Writer) (int, bool) {
	endpoint, err := remoteDaemonAPIBase(api)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2, true
	}
	if endpoint == "" {
		return 0, false
	}
	if supportsWatch && (*daemon || *agents) {
		// --daemon and --agents probe this host's own process, so an explicit
		// --api conflicts with them. An inherited $GOOBERS_DAEMON_API (worker
		// pods export it) must not break the local probe, so it falls back to
		// the local path instead.
		if strings.TrimSpace(api) != "" {
			pf(stderr, "error: --api cannot be combined with --daemon or --agents\n")
			return 2, true
		}
		return 0, false
	}
	remoteWatch, remoteInterval := false, defaultStatusWatchInterval
	if supportsWatch {
		remoteWatch, remoteInterval = *watch, *interval
	}
	return runRemoteRunTable(context.Background(), endpoint, root, rootExplicit, jsonOutput, remoteWatch, bounded, remoteInterval, options, stdout, stderr), true
}

// remoteStatusOmissionNote states what `status --api` cannot show yet: the
// daemon API serves the root identity, maintenance state and run list, but not
// the scheduler, fleet, queue, recovery or per-workflow detail the local status
// assembles from the instance root.
const remoteStatusOmissionNote = "note: over --api, status shows the root identity, maintenance state and run table only; " +
	"scheduler, fleet, queue, recovery and per-workflow detail are not served by the daemon API yet (#5985)\n"

// confirmRemoteReadIdentity runs confirmIdentity when reads is the remote
// adapter and is a no-op for the filesystem-backed reader.
func confirmRemoteReadIdentity(ctx context.Context, reads readservice.OfflineRuns) error {
	remote, ok := reads.(*remoteRuns)
	if !ok {
		return nil
	}
	_, err := remote.confirmIdentity(ctx)
	return err
}

// remoteTraceOmissionNote states what `trace --api` cannot show yet. Spans,
// agent progress, the reviewer's needs-changes rationale, recovery state and
// credit attribution have no daemon read route, so a remote trace omits them
// rather than rendering them empty as if the run had none.
const remoteTraceOmissionNote = "note: over --api, trace omits spans, agent progress, the escalation's needs-changes rationale, " +
	"recovery state, attribution, the journal machine state and journal-only event fields; the daemon API does not serve them yet (#5985)\n"

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
