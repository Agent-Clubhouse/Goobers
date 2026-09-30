package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ADOCIEvidenceBounds caps how much failure detail gather-ci-failures and
// ci-poll collect for one failing Azure DevOps build (#5652). A zero field
// takes its default.
type ADOCIEvidenceBounds struct {
	// FailedJobs caps the failed jobs (or, when no job failed, the failed
	// stages/phases) reported per build.
	FailedJobs int
	// FailedTasksPerJob caps the failed tasks reported per failed job.
	FailedTasksPerJob int
	// IssuesPerRecord caps the timeline issues (errors, else warnings)
	// reported per failed job or task.
	IssuesPerRecord int
	// LogLines caps the lines read from the tail of one step's log.
	LogLines int
	// ExcerptBytes caps the log excerpt kept per step.
	ExcerptBytes int
}

var defaultADOCIEvidenceBounds = ADOCIEvidenceBounds{
	FailedJobs:        3,
	FailedTasksPerJob: 3,
	IssuesPerRecord:   5,
	LogLines:          200,
	ExcerptBytes:      3000,
}

const (
	// adoLogChunkBytes splits a log excerpt into annotations no larger than
	// ci-poll's per-annotation message bound (1 KiB), so the excerpt reaches
	// ci-checks.json whole rather than cut to its first kilobyte.
	adoLogChunkBytes = 1000
	// adoLogResponseByteLimit bounds one log read however many lines ADO
	// sends back.
	adoLogResponseByteLimit = 1 << 20
	adoBuildAPIVersion      = "7.1"
)

func (p *ADOProvider) evidenceBounds() ADOCIEvidenceBounds {
	b := p.ciEvidenceBounds
	d := defaultADOCIEvidenceBounds
	pick := func(v, def int) int {
		if v > 0 {
			return v
		}
		return def
	}
	return ADOCIEvidenceBounds{
		FailedJobs:        pick(b.FailedJobs, d.FailedJobs),
		FailedTasksPerJob: pick(b.FailedTasksPerJob, d.FailedTasksPerJob),
		IssuesPerRecord:   pick(b.IssuesPerRecord, d.IssuesPerRecord),
		LogLines:          pick(b.LogLines, d.LogLines),
		ExcerptBytes:      pick(b.ExcerptBytes, d.ExcerptBytes),
	}
}

type adoBuild struct {
	ID            int               `json:"id"`
	BuildNumber   string            `json:"buildNumber"`
	Status        string            `json:"status"`
	Result        string            `json:"result"`
	SourceBranch  string            `json:"sourceBranch"`
	SourceVersion string            `json:"sourceVersion"`
	TriggerInfo   map[string]string `json:"triggerInfo"`
	Definition    struct {
		ID   json.Number `json:"id"`
		Name string      `json:"name"`
	} `json:"definition"`
	Repository struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"repository"`
}

type adoBuildsResponse struct {
	Value []adoBuild `json:"value"`
}

type adoTimeline struct {
	Records []adoTimelineRecord `json:"records"`
}

type adoTimelineRecord struct {
	ID       string             `json:"id"`
	ParentID string             `json:"parentId"`
	Type     string             `json:"type"`
	Name     string             `json:"name"`
	Result   string             `json:"result"`
	Order    int                `json:"order"`
	Issues   []adoTimelineIssue `json:"issues"`
	Log      *struct {
		ID int `json:"id"`
	} `json:"log"`
}

type adoTimelineIssue struct {
	Type    string         `json:"type"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data"`
}

type adoBuildLogsResponse struct {
	Value []struct {
		ID        int `json:"id"`
		LineCount int `json:"lineCount"`
	} `json:"value"`
}

type adoPRStatus struct {
	ID          int    `json:"id"`
	State       string `json:"state"`
	Description string `json:"description"`
	TargetURL   string `json:"targetUrl"`
	Context     struct {
		Name  string `json:"name"`
		Genre string `json:"genre"`
	} `json:"context"`
}

type adoPRStatusesResponse struct {
	Value []adoPRStatus `json:"value"`
}

// adoCIScope is the pull request one evidence collection diagnoses.
type adoCIScope struct {
	repo     RepositoryRef
	pullID   string
	detail   adoPullRequestDetail
	project  string
	statuses *[]adoPRStatus
}

// adoBuildRef names one build and the project that owns it.
type adoBuildRef struct {
	id      string
	project string
}

// adoCIEvidence accumulates the evidence for one failing policy.
type adoCIEvidence struct {
	state       CIEvidenceState
	notes       []string
	annotations []CheckAnnotation
	buildURL    string
}

func newADOCIEvidence() adoCIEvidence {
	return adoCIEvidence{state: CIEvidenceComplete, annotations: []CheckAnnotation{}}
}

var adoEvidenceRank = map[CIEvidenceState]int{
	CIEvidenceComplete:        0,
	CIEvidencePartialBound:    1,
	CIEvidencePartialProvider: 2,
	CIEvidenceUnsupported:     3,
	CIEvidenceStale:           4,
	CIEvidenceFailed:          5,
}

// degrade records note and lowers the evidence to state when state is worse.
func (e *adoCIEvidence) degrade(state CIEvidenceState, note string) {
	if adoEvidenceRank[state] > adoEvidenceRank[e.state] {
		e.state = state
	}
	if note != "" {
		e.notes = append(e.notes, note)
	}
}

// summary renders the grade and notes after the policy's own summary.
func (e adoCIEvidence) summary(prefix string) string {
	parts := append([]string{prefix, "evidence " + string(e.state)}, e.notes...)
	return strings.Join(parts, "; ")
}

// readFailure turns a failed evidence read into explicit failed evidence. An
// authentication failure or a cancelled context is returned instead, so a
// revoked credential never reads as a collected-but-empty diagnosis.
func adoEvidenceReadFailure(ctx context.Context, e *adoCIEvidence, what string, err error) error {
	if IsAuthenticationError(err) || ctx.Err() != nil {
		return err
	}
	e.degrade(CIEvidenceFailed, fmt.Sprintf("%s failed: %v", what, err))
	return nil
}

// ciEvidenceFor collects the diagnostics behind one failing CI policy: it
// finds the build the policy evaluated, checks that the build belongs to this
// pull request, and reads the failed timeline records and their log tails.
func (p *ADOProvider) ciEvidenceFor(ctx context.Context, s *adoCIScope, ev adoPolicyEvaluation) (adoCIEvidence, error) {
	e := newADOCIEvidence()
	ref, err := p.locateEvaluatedBuild(ctx, s, ev, &e)
	if err != nil || ref.id == "" {
		return e, err
	}
	e.buildURL = p.buildResultsURL(ref.project, ref.id)
	err = p.collectBuildEvidence(ctx, s, ref, &e)
	return e, err
}

// locateEvaluatedBuild resolves the build behind ev. A policy that has no
// Azure DevOps build behind it is graded unsupported, with the reason.
func (p *ADOProvider) locateEvaluatedBuild(ctx context.Context, s *adoCIScope, ev adoPolicyEvaluation, e *adoCIEvidence) (adoBuildRef, error) {
	switch strings.ToLower(strings.TrimSpace(ev.Configuration.Type.ID)) {
	case adoPolicyTypeBuild:
		return p.buildPolicyBuild(ctx, s, ev, e)
	case adoPolicyTypeStatus:
		return p.statusPolicyBuild(ctx, s, ev, e)
	default:
		e.degrade(CIEvidenceUnsupported, fmt.Sprintf(
			"unsupported evidence source: policy type %q is neither a build nor a status policy, so Azure DevOps exposes no failure detail for it",
			adoPolicyName(ev)))
		return adoBuildRef{}, nil
	}
}

// buildPolicyBuild is the build a build policy evaluated: context.buildId,
// or else the latest build of the policy's definition for the pull request's
// merge ref.
func (p *ADOProvider) buildPolicyBuild(ctx context.Context, s *adoCIScope, ev adoPolicyEvaluation, e *adoCIEvidence) (adoBuildRef, error) {
	if id := ev.Context.BuildID.String(); id != "" {
		return adoBuildRef{id: id, project: s.project}, nil
	}
	definition := ev.Configuration.Settings.BuildDefinitionID.String()
	if definition == "" {
		e.degrade(CIEvidencePartialProvider, "the evaluation names no build and the policy names no build definition")
		return adoBuildRef{}, nil
	}
	build, found, err := p.latestPullRequestBuild(ctx, s, definition)
	if err != nil {
		return adoBuildRef{}, adoEvidenceReadFailure(ctx, e, "build lookup", err)
	}
	if !found {
		e.degrade(CIEvidencePartialProvider, fmt.Sprintf("no build of definition %s was found for refs/pull/%s/merge", definition, s.pullID))
		return adoBuildRef{}, nil
	}
	return adoBuildRef{id: strconv.Itoa(build.ID), project: s.project}, nil
}

// latestPullRequestBuild reads the most recently queued build of definition
// for the pull request's merge ref.
func (p *ADOProvider) latestPullRequestBuild(ctx context.Context, s *adoCIScope, definition string) (adoBuild, bool, error) {
	endpoint, err := p.buildURL(s.project, url.Values{
		"definitions": []string{definition},
		"branchName":  []string{"refs/pull/" + s.pullID + "/merge"},
		"queryOrder":  []string{"queueTimeDescending"},
		"$top":        []string{"1"},
	})
	if err != nil {
		return adoBuild{}, false, err
	}
	var out adoBuildsResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return adoBuild{}, false, err
	}
	if len(out.Value) == 0 {
		return adoBuild{}, false, nil
	}
	return out.Value[0], true, nil
}

// statusPolicyBuild finds the build behind a status policy through the pull
// request status it requires: a status whose target is a build of this
// organization. Any other status comes from an external service, whose
// failure detail Azure DevOps cannot supply.
func (p *ADOProvider) statusPolicyBuild(ctx context.Context, s *adoCIScope, ev adoPolicyEvaluation, e *adoCIEvidence) (adoBuildRef, error) {
	settings := ev.Configuration.Settings
	label := strings.Trim(settings.StatusGenre+"/"+settings.StatusName, "/")
	status, found, err := p.latestPullRequestStatus(ctx, s, settings.StatusGenre, settings.StatusName)
	if err != nil {
		return adoBuildRef{}, adoEvidenceReadFailure(ctx, e, "pull request status read", err)
	}
	if !found {
		e.degrade(CIEvidenceUnsupported, fmt.Sprintf(
			"unsupported evidence source: no pull request status %q was found, so there is no build to read", label))
		return adoBuildRef{}, nil
	}
	if d := strings.TrimSpace(status.Description); d != "" {
		e.notes = append(e.notes, "status description: "+d)
	}
	ref, ok := p.buildRefFromTargetURL(status.TargetURL)
	if !ok {
		e.degrade(CIEvidenceUnsupported, fmt.Sprintf(
			"unsupported evidence source: status %q targets %q, which is not an Azure DevOps build of this organization; failure detail from an external status is not collected",
			label, status.TargetURL))
		return adoBuildRef{}, nil
	}
	return ref, nil
}

// latestPullRequestStatus returns the most recent status named genre/name.
// The statuses are read once per collection.
func (p *ADOProvider) latestPullRequestStatus(ctx context.Context, s *adoCIScope, genre, name string) (adoPRStatus, bool, error) {
	if s.statuses == nil {
		endpoint, err := p.repoURL(s.repo, "pullRequests", s.pullID, "statuses")
		if err != nil {
			return adoPRStatus{}, false, err
		}
		var out adoPRStatusesResponse
		if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
			return adoPRStatus{}, false, err
		}
		s.statuses = &out.Value
	}
	var latest adoPRStatus
	found := false
	for _, st := range *s.statuses {
		if !strings.EqualFold(st.Context.Name, name) || !strings.EqualFold(st.Context.Genre, genre) {
			continue
		}
		if !found || st.ID > latest.ID {
			latest, found = st, true
		}
	}
	return latest, found && strings.TrimSpace(name) != "", nil
}

// buildRefFromTargetURL recognizes a build-results link of this provider's
// host and organization: <base>/<org>/<project>/_build/results?buildId=N.
func (p *ADOProvider) buildRefFromTargetURL(target string) (adoBuildRef, bool) {
	u, err := url.Parse(strings.TrimSpace(target))
	if err != nil || u.Host == "" {
		return adoBuildRef{}, false
	}
	base, err := url.Parse(p.webBaseURL())
	if err != nil || !strings.EqualFold(u.Host, base.Host) {
		return adoBuildRef{}, false
	}
	rest := strings.TrimPrefix(strings.Trim(u.Path, "/"), strings.Trim(base.Path, "/"))
	segments := strings.Split(strings.Trim(rest, "/"), "/")
	if len(segments) < 3 || !strings.EqualFold(segments[0], p.Organization) || segments[2] != "_build" {
		return adoBuildRef{}, false
	}
	id := u.Query().Get("buildId")
	if _, err := strconv.Atoi(id); err != nil {
		return adoBuildRef{}, false
	}
	project, err := url.PathUnescape(segments[1])
	if err != nil {
		return adoBuildRef{}, false
	}
	return adoBuildRef{id: id, project: project}, true
}

func (p *ADOProvider) webBaseURL() string {
	if base := strings.TrimSuffix(p.BaseURL, "/"); base != "" {
		return base
	}
	return "https://dev.azure.com"
}

// buildURL builds a build-API endpoint under project.
func (p *ADOProvider) buildURL(project string, query url.Values, elems ...string) (string, error) {
	parts := append([]string{p.Organization, project, "_apis", "build", "builds"}, elems...)
	endpoint, err := joinURL(p.BaseURL, parts...)
	if err != nil {
		return "", err
	}
	if query == nil {
		query = url.Values{}
	}
	query.Set("api-version", adoBuildAPIVersion)
	return addQuery(endpoint, query)
}

// collectBuildEvidence reads one build, checks its identity against the pull
// request, and gathers its failed timeline records.
func (p *ADOProvider) collectBuildEvidence(ctx context.Context, s *adoCIScope, ref adoBuildRef, e *adoCIEvidence) error {
	var build adoBuild
	endpoint, err := p.buildURL(ref.project, nil, ref.id)
	if err == nil {
		err = p.do(ctx, http.MethodGet, endpoint, nil, &build)
	}
	if err != nil {
		return adoEvidenceReadFailure(ctx, e, "build "+ref.id+" read", err)
	}
	e.notes = append(e.notes, adoBuildDescription(build))
	if !adoCheckBuildIdentity(s, build, e) {
		return nil
	}
	var timeline adoTimeline
	endpoint, err = p.buildURL(ref.project, nil, ref.id, "timeline")
	if err == nil {
		err = p.do(ctx, http.MethodGet, endpoint, nil, &timeline)
	}
	if err != nil {
		return adoEvidenceReadFailure(ctx, e, "build "+ref.id+" timeline read", err)
	}
	return p.collectTimelineEvidence(ctx, ref, timeline, e)
}

func adoBuildDescription(b adoBuild) string {
	definition := strings.TrimSpace(b.Definition.Name)
	if id := b.Definition.ID.String(); id != "" {
		definition = strings.TrimSpace(definition + " #" + id)
	}
	return fmt.Sprintf("build %s (id %d, definition %s) %s/%s on %s at %s",
		b.BuildNumber, b.ID, definition, b.Status, b.Result, b.SourceBranch, b.SourceVersion)
}

// adoCheckBuildIdentity validates that build ran for this pull request's
// repository and pull request (rejecting it otherwise) and grades a build of
// another source head stale. It reports whether the build's detail may be
// collected.
func adoCheckBuildIdentity(s *adoCIScope, b adoBuild, e *adoCIEvidence) bool {
	wantRepo := s.detail.Repository.ID
	if b.Repository.ID != "" && wantRepo != "" && !strings.EqualFold(b.Repository.ID, wantRepo) {
		e.degrade(CIEvidenceFailed, fmt.Sprintf("rejected: build %d ran for repository %s, not this pull request's repository %s", b.ID, b.Repository.ID, wantRepo))
		return false
	}
	if pr := strings.TrimSpace(b.TriggerInfo["pr.number"]); pr != "" && pr != s.pullID {
		e.degrade(CIEvidenceFailed, fmt.Sprintf("rejected: build %d ran for pull request %s, not %s", b.ID, pr, s.pullID))
		return false
	}
	head := s.detail.LastMergeSourceCommit.CommitID
	built := strings.TrimSpace(b.TriggerInfo["pr.sourceSha"])
	switch {
	case built == "":
		e.notes = append(e.notes, "the build does not report the pull request head it merged")
	case head != "" && !strings.EqualFold(built, head):
		e.degrade(CIEvidenceStale, fmt.Sprintf("STALE: build %d ran at pull request head %s, not the current head %s", b.ID, built, head))
	}
	return true
}

// adoFailedStep is one failed timeline record chosen for evidence.
type adoFailedStep struct {
	record adoTimelineRecord
	title  string
}

// collectTimelineEvidence turns the failed timeline records into annotations:
// each step's issues, then an excerpt of its log tail.
func (p *ADOProvider) collectTimelineEvidence(ctx context.Context, ref adoBuildRef, timeline adoTimeline, e *adoCIEvidence) error {
	bounds := p.evidenceBounds()
	steps, dropped := selectADOFailedSteps(timeline.Records, bounds)
	if len(steps) == 0 {
		e.degrade(CIEvidencePartialProvider, "the build timeline reports no failed job or task")
		return nil
	}
	e.notes = append(e.notes, fmt.Sprintf("%d failed step(s) reported", len(steps)))
	if dropped > 0 {
		e.degrade(CIEvidencePartialBound, fmt.Sprintf("%d further failed step(s) omitted by the collection bound", dropped))
	}
	logs := &adoLogCatalog{}
	for _, step := range steps {
		p.appendStepIssues(step, bounds, e)
		if err := p.appendStepLog(ctx, ref, step, bounds, logs, e); err != nil {
			return err
		}
	}
	return nil
}

func adoRecordFailed(r adoTimelineRecord) bool {
	switch strings.ToLower(r.Result) {
	case "failed", "canceled", "abandoned":
		return true
	default:
		return false
	}
}

// selectADOFailedSteps picks, in timeline order, each failed job's failed
// tasks (or the job itself when no task failed) within bounds. When no job
// failed it falls back to the other failed records (stages, phases,
// checkpoints). dropped counts the failed records the bounds left out.
func selectADOFailedSteps(records []adoTimelineRecord, bounds ADOCIEvidenceBounds) ([]adoFailedStep, int) {
	sorted := append([]adoTimelineRecord(nil), records...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Order < sorted[j].Order })
	jobs := filterADORecords(sorted, func(r adoTimelineRecord) bool { return r.Type == "Job" && adoRecordFailed(r) })
	if len(jobs) == 0 {
		other := filterADORecords(sorted, func(r adoTimelineRecord) bool { return r.Type != "Task" && adoRecordFailed(r) })
		kept := min(len(other), bounds.FailedJobs)
		steps := make([]adoFailedStep, 0, kept)
		for _, r := range other[:kept] {
			steps = append(steps, adoFailedStep{record: r, title: r.Type + " " + r.Name})
		}
		return steps, len(other) - kept
	}
	dropped := max(len(jobs)-bounds.FailedJobs, 0)
	var steps []adoFailedStep
	for _, job := range jobs[:min(len(jobs), bounds.FailedJobs)] {
		tasks := filterADORecords(sorted, func(r adoTimelineRecord) bool {
			return r.Type == "Task" && r.ParentID == job.ID && adoRecordFailed(r)
		})
		if len(tasks) == 0 {
			steps = append(steps, adoFailedStep{record: job, title: job.Name})
			continue
		}
		dropped += max(len(tasks)-bounds.FailedTasksPerJob, 0)
		for _, task := range tasks[:min(len(tasks), bounds.FailedTasksPerJob)] {
			steps = append(steps, adoFailedStep{record: task, title: job.Name + " / " + task.Name})
		}
	}
	return steps, dropped
}

func filterADORecords(records []adoTimelineRecord, keep func(adoTimelineRecord) bool) []adoTimelineRecord {
	var out []adoTimelineRecord
	for _, r := range records {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// appendStepIssues records a step's error issues (its warnings when it has
// none) as annotations, within the per-record bound.
func (p *ADOProvider) appendStepIssues(step adoFailedStep, bounds ADOCIEvidenceBounds, e *adoCIEvidence) {
	issues := adoIssuesOfType(step.record.Issues, "error")
	if len(issues) == 0 {
		issues = adoIssuesOfType(step.record.Issues, "warning")
	}
	if len(issues) > bounds.IssuesPerRecord {
		e.degrade(CIEvidencePartialBound, fmt.Sprintf("%s: %d further issue(s) omitted by the collection bound", step.title, len(issues)-bounds.IssuesPerRecord))
		issues = issues[:bounds.IssuesPerRecord]
	}
	for _, issue := range issues {
		e.annotations = append(e.annotations, CheckAnnotation{
			Path:      adoIssueData(issue, "sourcepath"),
			StartLine: adoIssueLine(issue),
			Level:     strings.ToLower(issue.Type),
			Title:     step.title,
			Message:   issue.Message,
		})
	}
}

func adoIssuesOfType(issues []adoTimelineIssue, kind string) []adoTimelineIssue {
	var out []adoTimelineIssue
	for _, issue := range issues {
		if strings.EqualFold(issue.Type, kind) && strings.TrimSpace(issue.Message) != "" {
			out = append(out, issue)
		}
	}
	return out
}

func adoIssueData(issue adoTimelineIssue, key string) string {
	for k, v := range issue.Data {
		if strings.EqualFold(k, key) {
			return strings.TrimSpace(fmt.Sprint(v))
		}
	}
	return ""
}

func adoIssueLine(issue adoTimelineIssue) int {
	line, err := strconv.Atoi(adoIssueData(issue, "linenumber"))
	if err != nil || line < 0 {
		return 0
	}
	return line
}

// adoLogCatalog is a build's log line counts, read once, on first use.
type adoLogCatalog struct {
	loaded bool
	lines  map[int]int
}

// appendStepLog adds a bounded excerpt of the step's log tail. An unreadable
// log is graded partial_provider, never silently skipped.
func (p *ADOProvider) appendStepLog(ctx context.Context, ref adoBuildRef, step adoFailedStep, bounds ADOCIEvidenceBounds, logs *adoLogCatalog, e *adoCIEvidence) error {
	if step.record.Log == nil || step.record.Log.ID == 0 {
		e.degrade(CIEvidencePartialProvider, step.title+": no log is attached to this step")
		return nil
	}
	lines, truncated, err := p.readLogTail(ctx, ref, step.record.Log.ID, bounds.LogLines, logs)
	if err != nil {
		if IsAuthenticationError(err) || ctx.Err() != nil {
			return err
		}
		e.degrade(CIEvidencePartialProvider, fmt.Sprintf("%s: log %d read failed: %v", step.title, step.record.Log.ID, err))
		return nil
	}
	excerpt, cut := adoLogExcerpt(lines, bounds.ExcerptBytes)
	if truncated || cut {
		e.degrade(CIEvidencePartialBound, fmt.Sprintf("%s: log %d excerpt truncated to its last lines before the error", step.title, step.record.Log.ID))
	}
	if excerpt == "" {
		e.degrade(CIEvidencePartialProvider, fmt.Sprintf("%s: log %d is empty", step.title, step.record.Log.ID))
		return nil
	}
	for _, chunk := range adoChunkLines(excerpt, adoLogChunkBytes) {
		e.annotations = append(e.annotations, CheckAnnotation{
			Level: "error", Title: "log excerpt: " + step.title, Message: chunk,
		})
	}
	return nil
}

// readLogTail reads at most maxLines from the end of one build log. truncated
// reports that earlier lines were not read.
func (p *ADOProvider) readLogTail(ctx context.Context, ref adoBuildRef, logID, maxLines int, logs *adoLogCatalog) ([]string, bool, error) {
	if err := p.loadLogCatalog(ctx, ref, logs); err != nil {
		return nil, false, err
	}
	query := url.Values{}
	total := logs.lines[logID]
	if total > maxLines {
		query.Set("startLine", strconv.Itoa(total-maxLines+1))
		query.Set("endLine", strconv.Itoa(total))
	}
	endpoint, err := p.buildURL(ref.project, query, ref.id, "logs", strconv.Itoa(logID))
	if err != nil {
		return nil, false, err
	}
	body, err := p.readBounded(ctx, endpoint, adoLogResponseByteLimit)
	if err != nil {
		return nil, false, err
	}
	lines := decodeADOLogLines(body)
	truncated := total > maxLines
	if len(lines) > maxLines {
		lines, truncated = lines[len(lines)-maxLines:], true
	}
	return lines, truncated, nil
}

// loadLogCatalog reads the build's log list once for its line counts. A
// failed read leaves the counts unknown: each log is then read whole, within
// the response byte bound, and trimmed to its tail.
func (p *ADOProvider) loadLogCatalog(ctx context.Context, ref adoBuildRef, logs *adoLogCatalog) error {
	if logs.loaded {
		return nil
	}
	logs.loaded = true
	logs.lines = map[int]int{}
	endpoint, err := p.buildURL(ref.project, nil, ref.id, "logs")
	if err != nil {
		return err
	}
	var out adoBuildLogsResponse
	if err := p.do(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		if IsAuthenticationError(err) || ctx.Err() != nil {
			return err
		}
		return nil
	}
	for _, l := range out.Value {
		logs.lines[l.ID] = l.LineCount
	}
	return nil
}

// readBounded GETs endpoint through send (so rate-limit and 401 handling
// apply) and reads at most limit bytes of a successful body.
func (p *ADOProvider) readBounded(ctx context.Context, endpoint string, limit int64) ([]byte, error) {
	resp, err := p.send(ctx, http.MethodGet, endpoint, nil, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, newProviderResponseError(resp, http.MethodGet, endpoint, body)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	return body, nil
}

// decodeADOLogLines accepts both shapes ADO serves a log in: JSON
// {"count":n,"value":[lines]} for an application/json request, and plain
// text otherwise.
func decodeADOLogLines(body []byte) []string {
	var out struct {
		Value []string `json:"value"`
	}
	trimmed := strings.TrimSpace(string(body))
	if strings.HasPrefix(trimmed, "{") && json.Unmarshal(body, &out) == nil {
		return out.Value
	}
	if trimmed == "" {
		return nil
	}
	return strings.Split(strings.ReplaceAll(strings.TrimRight(string(body), "\n"), "\r\n", "\n"), "\n")
}

// adoLogExcerpt keeps the lines leading up to the log's last ##[error] line
// (its last line when there is none), newest last, within maxBytes. Each
// line loses its leading ADO timestamp. cut reports that earlier lines were
// dropped.
func adoLogExcerpt(lines []string, maxBytes int) (string, bool) {
	end := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "##[error]") {
			end = i + 1
			break
		}
	}
	var kept []string
	size := 0
	start := end
	for start > 0 {
		line := stripADOLogTimestamp(lines[start-1])
		if size+len(line)+1 > maxBytes {
			break
		}
		kept = append(kept, line)
		size += len(line) + 1
		start--
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return strings.TrimSpace(strings.Join(kept, "\n")), start > 0
}

// stripADOLogTimestamp drops the RFC 3339 timestamp ADO prefixes to each log
// line ("2026-01-02T03:04:05.1234567Z ").
func stripADOLogTimestamp(line string) string {
	line = strings.TrimRight(line, "\r")
	stamp, rest, ok := strings.Cut(line, " ")
	if ok && len(stamp) >= 20 && stamp[4] == '-' && stamp[10] == 'T' && strings.HasSuffix(stamp, "Z") {
		return rest
	}
	return line
}

// adoChunkLines splits text at line boundaries into chunks of at most limit
// bytes; a single longer line is cut.
func adoChunkLines(text string, limit int) []string {
	var chunks []string
	var current strings.Builder
	for _, line := range strings.Split(text, "\n") {
		for len(line) > limit {
			cut := limit
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			if cut == 0 {
				cut = limit
			}
			chunks = append(chunks, line[:cut])
			line = line[cut:]
		}
		if current.Len() > 0 && current.Len()+1+len(line) > limit {
			chunks = append(chunks, current.String())
			current.Reset()
		}
		if current.Len() > 0 {
			current.WriteByte('\n')
		}
		current.WriteString(line)
	}
	if current.Len() > 0 {
		chunks = append(chunks, current.String())
	}
	return chunks
}
