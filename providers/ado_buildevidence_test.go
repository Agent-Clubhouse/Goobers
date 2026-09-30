package providers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// #5652: gather-ci-failures and ci-poll on Azure DevOps trace each failing
// build or status policy to its build and report the failed jobs, tasks,
// issues and a bounded log excerpt, graded for completeness. These tests fake
// the ADO build REST shapes: builds/{id}, builds/{id}/timeline, builds/{id}/logs,
// builds/{id}/logs/{logId}, the builds list query and pull request statuses.

// adoBuildFake is one fake ADO organization serving PR 42 (head-sha), its
// policy evaluations and its builds.
type adoBuildFake struct {
	t           *testing.T
	evaluations []map[string]interface{}
	builds      map[string]map[string]interface{}
	timelines   map[string][]map[string]interface{}
	logCounts   map[string][]map[string]interface{}
	logs        map[string][]string
	statuses    []map[string]interface{}
	listBuilds  []map[string]interface{}
	failPaths   map[string]int

	mu       sync.Mutex
	requests []*http.Request
}

func newADOBuildFake(t *testing.T) *adoBuildFake {
	return &adoBuildFake{
		t:         t,
		builds:    map[string]map[string]interface{}{},
		timelines: map[string][]map[string]interface{}{},
		logCounts: map[string][]map[string]interface{}{},
		logs:      map[string][]string{},
		failPaths: map[string]int{},
	}
}

func (f *adoBuildFake) serve() (*ADOProvider, func()) {
	mux := http.NewServeMux()
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullrequests/42", prDetailHandler(f.t, nil))
	mux.HandleFunc("/org/project/_apis/policy/evaluations", policyEvaluationsHandler(f.t, f.evaluations))
	mux.HandleFunc("/org/project/_apis/git/repositories/repo/pullRequests/42/statuses", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(f.t, w, map[string]interface{}{"value": f.statuses})
	})
	mux.HandleFunc("/org/project/_apis/build/builds", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(f.t, w, map[string]interface{}{"value": f.listBuilds})
	})
	mux.HandleFunc("/org/project/_apis/build/builds/", f.buildHandler)
	server := httptest.NewServer(f.record(mux))
	provider := NewADOProvider("org", "project", "token", func(p *ADOProvider) { p.BaseURL = server.URL })
	return provider, server.Close
}

func (f *adoBuildFake) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r)
		f.mu.Unlock()
		if r.Method != http.MethodGet {
			f.t.Errorf("request %s %s: CI evidence collection must be read-only", r.Method, r.URL.Path)
		}
		if code := f.failPaths[r.URL.Path]; code != 0 {
			w.WriteHeader(code)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// buildHandler serves builds/{id}[/timeline|/logs[/{logId}]].
func (f *adoBuildFake) buildHandler(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/org/project/_apis/build/builds/"), "/")
	id := parts[0]
	switch {
	case len(parts) == 1 && f.builds[id] != nil:
		writeJSON(f.t, w, f.builds[id])
	case len(parts) == 2 && parts[1] == "timeline" && f.timelines[id] != nil:
		writeJSON(f.t, w, map[string]interface{}{"records": f.timelines[id]})
	case len(parts) == 2 && parts[1] == "logs":
		writeJSON(f.t, w, map[string]interface{}{"value": f.logCounts[id]})
	case len(parts) == 3 && parts[1] == "logs" && f.logs[id+"/"+parts[2]] != nil:
		writeJSON(f.t, w, map[string]interface{}{"count": len(f.logs[id+"/"+parts[2]]), "value": f.logs[id+"/"+parts[2]]})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *adoBuildFake) requested(path string) *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.requests {
		if r.URL.Path == path {
			return r
		}
	}
	return nil
}

func (f *adoBuildFake) collect(t *testing.T, provider *ADOProvider) []CIFailureDetail {
	t.Helper()
	got, err := provider.PullRequestCIFailures(context.Background(), RepositoryRef{Name: "repo", Project: "project"}, "42")
	if err != nil {
		t.Fatalf("PullRequestCIFailures: %v", err)
	}
	return got.Failures
}

// prBuild is build id for PR 42 of repo-guid, built at prHead.
func prBuild(id int, prHead string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "buildNumber": fmt.Sprintf("20260101.%d", id), "status": "completed", "result": "failed",
		"sourceBranch": "refs/pull/42/merge", "sourceVersion": "merge-sha",
		"triggerInfo": map[string]string{"pr.number": "42", "pr.sourceSha": prHead},
		"definition":  map[string]interface{}{"id": 12, "name": "ci"},
		"repository":  map[string]string{"id": "repo-guid", "name": "repo"},
	}
}

func timelineRecord(id, parent, kind, name, result string, order, logID int, issues ...map[string]interface{}) map[string]interface{} {
	record := map[string]interface{}{
		"id": id, "parentId": parent, "type": kind, "name": name, "result": result, "order": order, "issues": issues,
	}
	if logID != 0 {
		record["log"] = map[string]int{"id": logID}
	}
	return record
}

func adoIssue(kind, message string, data map[string]string) map[string]interface{} {
	return map[string]interface{}{"type": kind, "category": "General", "message": message, "data": data}
}

// failedLinuxTimeline is one failed job ("Linux") whose "go test" task failed
// with a compile error and its log (id 7), beside a passing job.
func failedLinuxTimeline() []map[string]interface{} {
	return []map[string]interface{}{
		timelineRecord("stage", "", "Stage", "Validate", "failed", 1, 0),
		timelineRecord("job-linux", "stage", "Job", "Linux", "failed", 1, 3),
		timelineRecord("t-checkout", "job-linux", "Task", "Checkout", "succeeded", 1, 5),
		timelineRecord("t-test", "job-linux", "Task", "go test", "failed", 2, 7,
			adoIssue("warning", "deprecated flag", nil),
			adoIssue("error", "undefined: widgetCount", map[string]string{"sourcepath": "widget/widget.go", "linenumber": "42"}),
			adoIssue("error", "Bash exited with code '1'.", nil)),
		timelineRecord("job-windows", "stage", "Job", "Windows", "succeeded", 2, 9),
	}
}

func goTestLogLines() []string {
	return []string{
		"2026-01-01T00:00:01.0000000Z ##[section]Starting: go test",
		"2026-01-01T00:00:02.0000000Z --- FAIL: TestWidget (0.00s)",
		"2026-01-01T00:00:02.1000000Z     widget_test.go:9: got 3, want 4",
		"2026-01-01T00:00:03.0000000Z FAIL\texample.com/widget\t0.01s",
		"2026-01-01T00:00:04.0000000Z ##[error]Bash exited with code '1'.",
		"2026-01-01T00:00:05.0000000Z ##[section]Finishing: go test",
	}
}

func rejectedBuildFake(t *testing.T, prHead string, lineCount int) *adoBuildFake {
	f := newADOBuildFake(t)
	f.evaluations = []map[string]interface{}{buildPolicy("rejected", 314)}
	f.builds["314"] = prBuild(314, prHead)
	f.timelines["314"] = failedLinuxTimeline()
	f.logCounts["314"] = []map[string]interface{}{{"id": 7, "lineCount": lineCount}}
	f.logs["314/7"] = goTestLogLines()
	return f
}

func TestADOCIEvidenceReportsFailedTasksIssuesAndLogExcerpt(t *testing.T) {
	f := rejectedBuildFake(t, "head-sha", len(goTestLogLines()))
	provider, done := f.serve()
	defer done()

	failures := f.collect(t, provider)
	if len(failures) != 1 {
		t.Fatalf("failures = %+v, want the rejected build", failures)
	}
	got := failures[0]
	if got.Evidence != CIEvidenceComplete {
		t.Errorf("evidence = %q, want complete; summary %q", got.Evidence, got.Summary)
	}
	for _, want := range []string{"evidence complete", "build 20260101.314", "ci #12", "1 failed step(s)"} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("summary = %q, want it to contain %q", got.Summary, want)
		}
	}
	if len(got.Annotations) != 3 {
		t.Fatalf("annotations = %+v, want the task's two errors (not its warning) and one log excerpt", got.Annotations)
	}
	issue := got.Annotations[0]
	if issue.Title != "Linux / go test" || issue.Level != "error" || issue.Path != "widget/widget.go" ||
		issue.StartLine != 42 || issue.Message != "undefined: widgetCount" {
		t.Errorf("issue annotation = %+v", issue)
	}
	excerpt := got.Annotations[2]
	if excerpt.Title != "log excerpt: Linux / go test" {
		t.Errorf("excerpt title = %q", excerpt.Title)
	}
	if !strings.Contains(excerpt.Message, "--- FAIL: TestWidget") || !strings.HasSuffix(excerpt.Message, "##[error]Bash exited with code '1'.") {
		t.Errorf("excerpt = %q, want the output leading up to the last ##[error] line", excerpt.Message)
	}
	if strings.Contains(excerpt.Message, "2026-01-01T") || strings.Contains(excerpt.Message, "Finishing") {
		t.Errorf("excerpt = %q, want timestamps stripped and nothing after the error", excerpt.Message)
	}
	if r := f.requested("/org/project/_apis/build/builds/314/logs/7"); r == nil || r.URL.Query().Get("startLine") != "" {
		t.Errorf("log request = %v, want the whole short log read without a range", r)
	}
}

// A long log is read from its tail only, and the excerpt says so.
func TestADOCIEvidenceReadsOnlyTheLogTail(t *testing.T) {
	f := rejectedBuildFake(t, "head-sha", 500)
	provider, done := f.serve()
	defer done()

	got := f.collect(t, provider)[0]
	r := f.requested("/org/project/_apis/build/builds/314/logs/7")
	if r == nil || r.URL.Query().Get("startLine") != "301" || r.URL.Query().Get("endLine") != "500" {
		t.Fatalf("log request = %v, want lines 301-500 (the default 200-line tail)", r)
	}
	if got.Evidence != CIEvidencePartialBound || !strings.Contains(got.Summary, "excerpt truncated") {
		t.Errorf("evidence = %q summary = %q, want partial_bound naming the truncation", got.Evidence, got.Summary)
	}
}

func TestADOCIEvidenceBoundsExcerptBytesAndChunks(t *testing.T) {
	f := rejectedBuildFake(t, "head-sha", 3000)
	lines := make([]string, 0, 3000)
	for i := range 3000 {
		lines = append(lines, fmt.Sprintf("line %04d %s", i, strings.Repeat("x", 40)))
	}
	f.logs["314/7"] = append(lines, "##[error]boom")
	provider, done := f.serve()
	defer done()

	got := f.collect(t, provider)[0]
	total := 0
	for _, a := range got.Annotations {
		if !strings.HasPrefix(a.Title, "log excerpt") {
			continue
		}
		if len(a.Message) > adoLogChunkBytes {
			t.Errorf("excerpt chunk = %d bytes, want <= %d", len(a.Message), adoLogChunkBytes)
		}
		total += len(a.Message)
	}
	if total == 0 || total > defaultADOCIEvidenceBounds.ExcerptBytes {
		t.Errorf("excerpt total = %d bytes, want 1..%d", total, defaultADOCIEvidenceBounds.ExcerptBytes)
	}
	if got.Evidence != CIEvidencePartialBound {
		t.Errorf("evidence = %q, want partial_bound", got.Evidence)
	}
}

// A build of an older PR head is still reported, but graded stale.
func TestADOCIEvidenceMarksBuildOfAnotherHeadStale(t *testing.T) {
	f := rejectedBuildFake(t, "old-sha", 6)
	provider, done := f.serve()
	defer done()

	got := f.collect(t, provider)[0]
	if got.Evidence != CIEvidenceStale || !strings.Contains(got.Summary, "STALE: build 314 ran at pull request head old-sha, not the current head head-sha") {
		t.Errorf("evidence = %q summary = %q, want stale naming both heads", got.Evidence, got.Summary)
	}
	if len(got.Annotations) == 0 {
		t.Error("annotations empty, want the stale build's diagnostics kept and graded")
	}
}

// A build of another repository or pull request is rejected: none of its
// detail is read or reported.
func TestADOCIEvidenceRejectsBuildOfAnotherRepositoryOrPullRequest(t *testing.T) {
	for name, mutate := range map[string]func(map[string]interface{}){
		"repository":   func(b map[string]interface{}) { b["repository"] = map[string]string{"id": "other-repo-guid"} },
		"pull request": func(b map[string]interface{}) { b["triggerInfo"] = map[string]string{"pr.number": "41"} },
	} {
		t.Run(name, func(t *testing.T) {
			f := rejectedBuildFake(t, "head-sha", 6)
			mutate(f.builds["314"])
			provider, done := f.serve()
			defer done()

			got := f.collect(t, provider)[0]
			if got.Evidence != CIEvidenceFailed || !strings.Contains(got.Summary, "rejected: build 314 ran for") {
				t.Errorf("evidence = %q summary = %q, want the build rejected", got.Evidence, got.Summary)
			}
			if len(got.Annotations) != 0 || f.requested("/org/project/_apis/build/builds/314/timeline") != nil {
				t.Errorf("annotations = %+v, want nothing read from a rejected build", got.Annotations)
			}
		})
	}
}

func TestADOCIEvidenceStatusPolicy(t *testing.T) {
	status := func(genre, name string) map[string]interface{} {
		ev := typedPolicy(adoPolicyTypeStatus, "Status", "rejected")
		ev["configuration"].(map[string]interface{})["settings"] = map[string]interface{}{"statusGenre": genre, "statusName": name}
		return ev
	}
	t.Run("external status is unsupported", func(t *testing.T) {
		f := newADOBuildFake(t)
		f.evaluations = []map[string]interface{}{status("ext-ci", "lint")}
		f.statuses = []map[string]interface{}{{
			"id": 3, "state": "failed", "description": "lint failed", "targetUrl": "https://ci.example.com/run/9",
			"context": map[string]string{"genre": "ext-ci", "name": "lint"},
		}}
		provider, done := f.serve()
		defer done()

		got := f.collect(t, provider)[0]
		if got.Evidence != CIEvidenceUnsupported || !strings.Contains(got.Summary, "unsupported evidence source") ||
			!strings.Contains(got.Summary, "https://ci.example.com/run/9") || !strings.Contains(got.Summary, "lint failed") {
			t.Errorf("evidence = %q summary = %q, want an explicit unsupported diagnostic", got.Evidence, got.Summary)
		}
	})
	t.Run("status posted by a build of this organization", func(t *testing.T) {
		f := rejectedBuildFake(t, "head-sha", 6)
		f.evaluations = []map[string]interface{}{status("pipelines", "validate")}
		provider, done := f.serve()
		defer done()
		f.statuses = []map[string]interface{}{
			{"id": 1, "targetUrl": provider.BaseURL + "/org/project/_build/results?buildId=99", "context": map[string]string{"genre": "pipelines", "name": "validate"}},
			{"id": 2, "targetUrl": provider.BaseURL + "/org/project/_build/results?buildId=314", "context": map[string]string{"genre": "pipelines", "name": "validate"}},
			{"id": 5, "targetUrl": provider.BaseURL + "/org/project/_build/results?buildId=77", "context": map[string]string{"genre": "pipelines", "name": "other"}},
		}

		got := f.collect(t, provider)[0]
		if got.Evidence != CIEvidenceComplete || len(got.Annotations) != 3 {
			t.Errorf("evidence = %q annotations = %+v, want the latest matching status's build read", got.Evidence, got.Annotations)
		}
		if want := provider.BaseURL + "/org/project/_build/results?buildId=314"; got.URL != want {
			t.Errorf("URL = %q, want %q", got.URL, want)
		}
	})
}

// A build policy whose evaluation names no build is traced through its
// definition's latest build of the pull request's merge ref.
func TestADOCIEvidenceFindsBuildByDefinition(t *testing.T) {
	f := rejectedBuildFake(t, "head-sha", 6)
	ev := typedPolicy(adoPolicyTypeBuild, "Build", "rejected")
	ev["configuration"].(map[string]interface{})["settings"] = map[string]interface{}{"buildDefinitionId": 12}
	f.evaluations = []map[string]interface{}{ev}
	f.listBuilds = []map[string]interface{}{prBuild(314, "head-sha")}
	provider, done := f.serve()
	defer done()

	got := f.collect(t, provider)[0]
	q := f.requested("/org/project/_apis/build/builds").URL.Query()
	if q.Get("definitions") != "12" || q.Get("branchName") != "refs/pull/42/merge" || q.Get("$top") != "1" {
		t.Errorf("build query = %v", q)
	}
	if got.Evidence != CIEvidenceComplete || len(got.Annotations) != 3 {
		t.Errorf("evidence = %q annotations = %+v, want the found build's detail", got.Evidence, got.Annotations)
	}
	if !strings.HasSuffix(got.URL, "_build/results?buildId=314") {
		t.Errorf("URL = %q, want the found build's link", got.URL)
	}
}

func TestADOCIEvidenceMissingDetailIsExplicit(t *testing.T) {
	cases := map[string]struct {
		mutate func(*adoBuildFake)
		want   CIEvidenceState
		note   string
	}{
		"no failed step": {func(f *adoBuildFake) {
			f.timelines["314"] = []map[string]interface{}{timelineRecord("j", "", "Job", "Linux", "succeeded", 1, 3)}
		}, CIEvidencePartialProvider, "no failed job or task"},
		"log unreadable": {func(f *adoBuildFake) {
			f.failPaths["/org/project/_apis/build/builds/314/logs/7"] = http.StatusNotFound
		}, CIEvidencePartialProvider, "log 7 read failed"},
		"timeline unreadable": {func(f *adoBuildFake) {
			f.failPaths["/org/project/_apis/build/builds/314/timeline"] = http.StatusNotFound
		}, CIEvidenceFailed, "timeline read failed"},
		"no build found": {func(f *adoBuildFake) {
			ev := typedPolicy(adoPolicyTypeBuild, "Build", "rejected")
			ev["configuration"].(map[string]interface{})["settings"] = map[string]interface{}{"buildDefinitionId": 12}
			f.evaluations = []map[string]interface{}{ev}
		}, CIEvidencePartialProvider, "no build of definition 12"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := rejectedBuildFake(t, "head-sha", 6)
			tc.mutate(f)
			provider, done := f.serve()
			defer done()

			got := f.collect(t, provider)[0]
			if got.Evidence != tc.want || !strings.Contains(got.Summary, tc.note) {
				t.Errorf("evidence = %q summary = %q, want %q naming %q", got.Evidence, got.Summary, tc.want, tc.note)
			}
		})
	}
}

// An authentication failure while collecting detail is an error, never a
// failure reported with empty evidence.
func TestADOCIEvidenceAuthenticationFailureIsAnError(t *testing.T) {
	f := rejectedBuildFake(t, "head-sha", 6)
	f.failPaths["/org/project/_apis/build/builds/314/timeline"] = http.StatusUnauthorized
	provider, done := f.serve()
	defer done()

	_, err := provider.PullRequestCIFailures(context.Background(), RepositoryRef{Name: "repo", Project: "project"}, "42")
	if err == nil || !IsAuthenticationError(err) {
		t.Fatalf("err = %v, want an authentication error", err)
	}
}

func TestADOCIEvidenceBoundsFailedJobsTasksAndIssues(t *testing.T) {
	f := rejectedBuildFake(t, "head-sha", 6)
	var issues []map[string]interface{}
	for i := range 8 {
		issues = append(issues, adoIssue("error", fmt.Sprintf("error %d", i), nil))
	}
	f.timelines["314"] = []map[string]interface{}{
		timelineRecord("j1", "", "Job", "Linux", "failed", 1, 0),
		timelineRecord("t1", "j1", "Task", "step one", "failed", 1, 7, issues...),
		timelineRecord("t2", "j1", "Task", "step two", "failed", 2, 7),
		timelineRecord("j2", "", "Job", "Windows", "failed", 2, 0),
	}
	provider, done := f.serve()
	defer done()
	provider.ciEvidenceBounds = ADOCIEvidenceBounds{FailedJobs: 1, FailedTasksPerJob: 1, IssuesPerRecord: 2}

	got := f.collect(t, provider)[0]
	if got.Evidence != CIEvidencePartialBound {
		t.Errorf("evidence = %q, want partial_bound", got.Evidence)
	}
	for _, want := range []string{"2 further failed step(s) omitted", "6 further issue(s) omitted"} {
		if !strings.Contains(got.Summary, want) {
			t.Errorf("summary = %q, want %q", got.Summary, want)
		}
	}
	for _, a := range got.Annotations {
		if a.Title != "Linux / step one" && a.Title != "log excerpt: Linux / step one" {
			t.Errorf("annotation %+v is outside the bounds", a)
		}
	}
}

// Passing and non-CI policies produce no evidence and cost no build reads.
func TestADOCIEvidencePassingPoliciesReadNoBuilds(t *testing.T) {
	f := newADOBuildFake(t)
	f.evaluations = []map[string]interface{}{
		buildPolicy("approved", 314),
		typedPolicy(adoPolicyTypeMergeStrategy, "Require a merge strategy", "rejected"),
		typedPolicy(adoPolicyTypeCommentRequirements, "Comment requirements", "rejected"),
	}
	provider, done := f.serve()
	defer done()

	if failures := f.collect(t, provider); len(failures) != 0 {
		t.Fatalf("failures = %+v, want none", failures)
	}
	if r := f.requested("/org/project/_apis/build/builds/314"); r != nil {
		t.Errorf("read %s for a passing policy", r.URL.Path)
	}
}

func TestADOBuildRefFromTargetURL(t *testing.T) {
	p := NewADOProvider("org", "project", "token")
	cases := map[string]bool{
		"https://dev.azure.com/org/project/_build/results?buildId=5":        true,
		"https://dev.azure.com/ORG/My%20Project/_build/results?buildId=5":   true,
		"https://dev.azure.com/other/project/_build/results?buildId=5":      false,
		"https://ci.example.com/org/project/_build/results?buildId=5":       false,
		"https://dev.azure.com/org/project/_build/results?buildId=notanint": false,
		"https://dev.azure.com/org/project/_release?releaseId=5":            false,
		"": false,
	}
	for target, want := range cases {
		ref, ok := p.buildRefFromTargetURL(target)
		if ok != want {
			t.Errorf("%q: ok = %v, want %v (ref %+v)", target, ok, want, ref)
		}
	}
	if ref, _ := p.buildRefFromTargetURL("https://dev.azure.com/org/My%20Project/_build/results?buildId=5"); ref.project != "My Project" || ref.id != "5" {
		t.Errorf("ref = %+v, want project %q build 5", ref, "My Project")
	}
}

func TestADOLogExcerptHelpers(t *testing.T) {
	excerpt, cut := adoLogExcerpt([]string{"a", "b", "##[error]x", "after"}, 100)
	if excerpt != "a\nb\n##[error]x" || cut {
		t.Errorf("excerpt = %q cut = %v", excerpt, cut)
	}
	excerpt, cut = adoLogExcerpt([]string{"aaaa", "bbbb", "cccc"}, 10)
	if excerpt != "bbbb\ncccc" || !cut {
		t.Errorf("bounded excerpt = %q cut = %v, want the newest lines within 10 bytes", excerpt, cut)
	}
	if got := stripADOLogTimestamp("2026-01-02T03:04:05.1234567Z hello world"); got != "hello world" {
		t.Errorf("strip = %q", got)
	}
	if got := stripADOLogTimestamp("no timestamp here"); got != "no timestamp here" {
		t.Errorf("strip = %q", got)
	}
	chunks := adoChunkLines(strings.Repeat("é", 700)+"\nshort", 1000)
	for _, c := range chunks {
		if len(c) > 1000 || !strings.HasPrefix(c, "é") && c != "short" && !strings.HasSuffix(c, "short") {
			t.Errorf("chunk = %d bytes %q...", len(c), c[:min(len(c), 10)])
		}
	}
	if lines := decodeADOLogLines([]byte("one\r\ntwo\n")); len(lines) != 2 || lines[1] != "two" {
		t.Errorf("text lines = %q", lines)
	}
}

// gather-pr-context's hasFailingCI read agrees with the evidence read but
// never touches a build.
func TestADOHasPullRequestCIFailuresReadsNoBuilds(t *testing.T) {
	for name, tc := range map[string]struct {
		evals []map[string]interface{}
		want  bool
	}{
		"rejected build": {[]map[string]interface{}{buildPolicy("rejected", 314)}, true},
		"only non-CI":    {[]map[string]interface{}{typedPolicy(adoPolicyTypeCommentRequirements, "Comment requirements", "rejected")}, false},
	} {
		t.Run(name, func(t *testing.T) {
			f := rejectedBuildFake(t, "head-sha", 6)
			f.evaluations = tc.evals
			provider, done := f.serve()
			defer done()
			got, err := provider.HasPullRequestCIFailures(context.Background(), RepositoryRef{Name: "repo", Project: "project"}, "42")
			if err != nil || got != tc.want {
				t.Fatalf("HasPullRequestCIFailures = %v, %v; want %v", got, err, tc.want)
			}
			if r := f.requested("/org/project/_apis/build/builds/314"); r != nil {
				t.Errorf("read %s, want no build read", r.URL.Path)
			}
			if failures := f.collect(t, provider); (len(failures) > 0) != tc.want {
				t.Errorf("PullRequestCIFailures = %+v, disagrees with HasPullRequestCIFailures", failures)
			}
		})
	}
}
