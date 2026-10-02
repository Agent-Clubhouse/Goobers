package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/executor"
	"github.com/goobers/goobers/providers"
)

// #6130: pr-comment-watch on Azure DevOps, against a fake of the ADO REST
// shapes it reads (connectionData, the active-PR list with includeLabels, PR
// threads) and writes (the PR-labels sub-endpoint: POST by name, DELETE by id).

const (
	adoWatchSelfID   = "self-guid"
	adoWatchHumanID  = "human-guid"
	adoWatchSelfName = "Pat Operator"
)

// adoWatchPR is one fake active pull request.
type adoWatchPR struct {
	head    string
	labels  map[string]string // label id -> name
	threads []map[string]any
}

type fakeADOCommentWatch struct {
	t        *testing.T
	repo     providers.RepositoryRef
	mu       sync.Mutex
	prs      map[int]*adoWatchPR
	nextID   int
	writes   []string
	selfName string
}

func newFakeADOCommentWatch(t *testing.T, repo providers.RepositoryRef) *fakeADOCommentWatch {
	return &fakeADOCommentWatch{t: t, repo: repo, prs: map[int]*adoWatchPR{}, selfName: adoWatchSelfName}
}

func (f *fakeADOCommentWatch) addPR(number int, head string, labels ...string) *adoWatchPR {
	pr := &adoWatchPR{head: head, labels: map[string]string{}}
	for _, name := range labels {
		f.nextID++
		pr.labels["label-"+strconv.Itoa(f.nextID)] = name
	}
	f.prs[number] = pr
	return pr
}

// adoWatchComment is one thread comment by authorID at minute offset from a
// fixed base, with ADO's display name deliberately the same for everyone: only
// the identity id may tell authors apart.
func adoWatchComment(id int, authorID, content string, minute int) map[string]any {
	return map[string]any{
		"id": id, "parentCommentId": 0, "content": content, "commentType": "text",
		"author":        map[string]string{"id": authorID, "displayName": adoWatchSelfName},
		"publishedDate": time.Date(2026, 9, 1, 10, minute, 0, 0, time.UTC).Format(time.RFC3339),
	}
}

func adoWatchThread(id int, comments ...map[string]any) map[string]any {
	return map[string]any{"id": id, "status": "active", "comments": comments}
}

func (f *fakeADOCommentWatch) labelNames(number int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, name := range f.prs[number].labels {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (f *fakeADOCommentWatch) takeWrites() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.writes
	f.writes = nil
	return out
}

func (f *fakeADOCommentWatch) server() *httptest.Server {
	t := f.t
	repoPath := "/" + f.repo.Owner + "/" + f.repo.Project + "/_apis/git/repositories/" + f.repo.Name + "/pullrequests"
	mux := http.NewServeMux()
	mux.HandleFunc("/"+f.repo.Owner+"/_apis/connectionData", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONResp(t, w, map[string]any{"authenticatedUser": map[string]any{"id": adoWatchSelfID, "providerDisplayName": f.selfName}})
	})
	mux.HandleFunc(repoPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("includeLabels") != "true" {
			t.Errorf("PR list without includeLabels: %s", r.URL.RawQuery)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		writeJSONResp(t, w, map[string]any{"value": f.prListJSON()})
	})
	mux.HandleFunc(repoPath+"/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.servePR(w, r, strings.Split(strings.TrimPrefix(r.URL.Path, repoPath+"/"), "/"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected ADO request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	return httptest.NewServer(mux)
}

func (f *fakeADOCommentWatch) prListJSON() []map[string]any {
	numbers := make([]int, 0, len(f.prs))
	for n := range f.prs {
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	out := make([]map[string]any, 0, len(numbers))
	for _, n := range numbers {
		pr := f.prs[n]
		labels := []map[string]string{}
		for id, name := range pr.labels {
			labels = append(labels, map[string]string{"id": id, "name": name})
		}
		out = append(out, map[string]any{
			"pullRequestId": n, "status": "active",
			"sourceRefName": "refs/heads/" + pr.head, "targetRefName": "refs/heads/main",
			"createdBy": map[string]string{"id": adoWatchSelfID, "displayName": adoWatchSelfName},
			"labels":    labels,
		})
	}
	return out
}

// servePR serves one PR's threads and its labels sub-endpoint.
func (f *fakeADOCommentWatch) servePR(w http.ResponseWriter, r *http.Request, rest []string) {
	t := f.t
	number, _ := strconv.Atoi(rest[0])
	pr := f.prs[number]
	if pr == nil || len(rest) < 2 {
		t.Errorf("unexpected PR request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	switch {
	case rest[1] == "threads" && r.Method == http.MethodGet:
		writeJSONResp(t, w, map[string]any{"value": pr.threads})
	case rest[1] == "labels" && len(rest) == 2 && r.Method == http.MethodGet:
		f.writeLabels(w, pr)
	case rest[1] == "labels" && len(rest) == 2 && r.Method == http.MethodPost:
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode label POST: %v", err)
		}
		f.nextID++
		id := "label-" + strconv.Itoa(f.nextID)
		pr.labels[id] = body["name"]
		f.writes = append(f.writes, "add:"+rest[0]+":"+body["name"])
		writeJSONResp(t, w, map[string]string{"id": id, "name": body["name"]})
	case rest[1] == "labels" && len(rest) == 3 && r.Method == http.MethodDelete:
		name, ok := pr.labels[rest[2]]
		if !ok {
			t.Errorf("DELETE of unknown label id %q (ADO deletes a colon-named label by id)", rest[2])
		}
		delete(pr.labels, rest[2])
		f.writes = append(f.writes, "remove:"+rest[0]+":"+name)
		w.WriteHeader(http.StatusNoContent)
	default:
		t.Errorf("unexpected PR request: %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeADOCommentWatch) writeLabels(w http.ResponseWriter, pr *adoWatchPR) {
	value := []map[string]string{}
	for id, name := range pr.labels {
		value = append(value, map[string]string{"id": id, "name": name})
	}
	writeJSONResp(f.t, w, map[string]any{"value": value})
}

// adoCommentWatchFixture routes pr-comment-watch at a fake ADO repository and
// returns the fake and the directory its result file lands in.
func adoCommentWatchFixture(t *testing.T, seed func(*fakeADOCommentWatch)) (string, *fakeADOCommentWatch, string) {
	t.Helper()
	root, repo := adoReviewThreadsStageFixture(t, "ado-comment-watch")
	fake := newFakeADOCommentWatch(t, repo)
	seed(fake)
	server := fake.server()
	t.Cleanup(server.Close)
	routeADOStageProvider(t, server.URL)
	return root, fake, commentWatchWorkDir(t)
}

func runADOCommentWatch(t *testing.T, root string) (string, string) {
	t.Helper()
	code, stdout, stderr := runArgs(t, "pr-comment-watch", root)
	if code != 0 {
		t.Fatalf("pr-comment-watch: code = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	return stdout, stderr
}

// ownMarked is a Goobers-authored body as an attributed daemon write stores it.
func ownMarked(body string) string {
	return stampOwnFixtureBody(body, "pull-request-comment")
}

// TestPRCommentWatchADOSameIdentityUnmarkedCommentIsHuman is the shared-identity
// case the stage exists for on ADO: Goobers runs as the operator's own login,
// so its marked verdict and the operator's later unmarked comment share an
// identity id. The unmarked comment is the human's and routes the PR.
func TestPRCommentWatchADOSameIdentityUnmarkedCommentIsHuman(t *testing.T) {
	root, fake, dir := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
		pr := f.addPR(7, "goobers/implementation/x")
		pr.threads = []map[string]any{
			adoWatchThread(1, adoWatchComment(1, adoWatchSelfID, ownMarked("verdict: needs-changes"), 0)),
			adoWatchThread(2, adoWatchComment(1, adoWatchSelfID, "please also rename the flag", 5)),
		}
	})
	runADOCommentWatch(t, root)
	if got := fake.labelNames(7); strings.Join(got, ",") != needsRemediationLabel {
		t.Fatalf("PR 7 labels = %v, want [%s]", got, needsRemediationLabel)
	}
	result := readCommentWatchResult(t, dir)
	if result.Labeled != 1 || result.IdentityMode != prCommentWatchIdentityShared || result.BotID != adoWatchSelfID {
		t.Fatalf("result = %+v, want labeled 1 in shared mode as %s", result, adoWatchSelfID)
	}
	if entry := result.PRs[0]; entry.CommentAuthorID != adoWatchSelfID || entry.CommentCreatedAt != "2026-09-01T10:05:00Z" {
		t.Fatalf("triggering comment = %+v, want the operator's unmarked comment", entry)
	}
}

// TestPRCommentWatchADOMarkedCommentIsGoobers: a marked comment is Goobers'
// own whoever posted it — the attribution footer and a review-thread response
// marker alike — so a newer marked response suppresses older human feedback.
func TestPRCommentWatchADOMarkedCommentIsGoobers(t *testing.T) {
	for name, body := range map[string]string{
		"attribution footer":            ownMarked("Addressed in the latest push."),
		"review-thread response marker": "Addressed in `abc`.\n\n" + reviewThreadResponseMarker("run-1", "7/3"),
	} {
		t.Run(name, func(t *testing.T) {
			root, fake, dir := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
				pr := f.addPR(7, "goobers/implementation/x")
				pr.threads = []map[string]any{
					adoWatchThread(3, adoWatchComment(1, adoWatchHumanID, "this leaks a file handle", 0), adoWatchComment(2, adoWatchSelfID, body, 5)),
				}
				pr.threads[0]["threadContext"] = map[string]any{"filePath": "/worker.go"}
			})
			runADOCommentWatch(t, root)
			if writes := fake.takeWrites(); len(writes) != 0 {
				t.Fatalf("label writes = %v, want none: Goobers' marked reply is newest", writes)
			}
			if result := readCommentWatchResult(t, dir); result.Scanned != 1 || result.Labeled != 0 {
				t.Fatalf("result = %+v, want scanned 1 labeled 0", result)
			}
		})
	}
}

// TestPRCommentWatchADOQuotedMarkerStaysHuman: a person quoting a Goobers
// comment (marker included) in a reply is still a person.
func TestPRCommentWatchADOQuotedMarkerStaysHuman(t *testing.T) {
	quoted := "> " + strings.ReplaceAll(ownMarked("verdict"), "\n", "\n> ") + "\n\nI disagree with this"
	root, fake, _ := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
		pr := f.addPR(7, "goobers/implementation/x")
		pr.threads = []map[string]any{
			adoWatchThread(1, adoWatchComment(1, adoWatchSelfID, ownMarked("verdict"), 0)),
			adoWatchThread(2, adoWatchComment(1, adoWatchHumanID, quoted, 5)),
		}
	})
	runADOCommentWatch(t, root)
	if got := fake.labelNames(7); strings.Join(got, ",") != needsRemediationLabel {
		t.Fatalf("PR 7 labels = %v, want the quoting reply to route it", got)
	}
}

// TestPRCommentWatchADOIgnoresSystemThreads: vote, push and policy-status
// threads (a CodeReviewThreadType property or a system comment), deleted
// comments and deleted threads all postdate Goobers' response here, and none
// of them may count as human feedback.
func TestPRCommentWatchADOIgnoresSystemThreads(t *testing.T) {
	root, fake, _ := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
		pr := f.addPR(7, "goobers/implementation/x")
		vote := adoWatchThread(4, adoWatchComment(1, adoWatchHumanID, "Reviewer voted -5", 10))
		vote["properties"] = map[string]any{"CodeReviewThreadType": map[string]string{"$type": "System.String", "$value": "VoteUpdate"}}
		policy := adoWatchComment(1, adoWatchHumanID, "Build failed", 11)
		policy["commentType"] = "system"
		deletedComment := adoWatchComment(2, adoWatchHumanID, "", 12)
		deletedComment["isDeleted"] = true
		deletedThread := adoWatchThread(6, adoWatchComment(1, adoWatchHumanID, "never mind", 13))
		deletedThread["isDeleted"] = true
		pr.threads = []map[string]any{
			adoWatchThread(1, adoWatchComment(1, adoWatchHumanID, "please fix", 0)),
			adoWatchThread(2, adoWatchComment(1, adoWatchSelfID, ownMarked("fixed"), 5), deletedComment),
			vote, adoWatchThread(5, policy), deletedThread,
		}
	})
	runADOCommentWatch(t, root)
	if writes := fake.takeWrites(); len(writes) != 0 {
		t.Fatalf("label writes = %v, want none: only system/deleted activity follows Goobers' response", writes)
	}
}

// TestPRCommentWatchADOWatermarkOrdering: the watermark uses ADO's server
// publish time, across general and file threads, and breaks an exact tie by
// thread id, then comment id — never by the order ADO listed the threads.
func TestPRCommentWatchADOWatermarkOrdering(t *testing.T) {
	cases := []struct {
		name    string
		threads func() []map[string]any
		routed  bool
	}{
		{"newer human file-thread reply routes", func() []map[string]any {
			file := adoWatchThread(9, adoWatchComment(1, adoWatchHumanID, "nit", 0), adoWatchComment(2, adoWatchHumanID, "still wrong", 20))
			file["threadContext"] = map[string]any{"filePath": "/a.go"}
			return []map[string]any{file, adoWatchThread(2, adoWatchComment(1, adoWatchSelfID, ownMarked("done"), 10))}
		}, true},
		{"newer Goobers response suppresses, whatever the list order", func() []map[string]any {
			return []map[string]any{
				adoWatchThread(2, adoWatchComment(1, adoWatchSelfID, ownMarked("done"), 30)),
				adoWatchThread(9, adoWatchComment(1, adoWatchHumanID, "please fix", 20)),
			}
		}, false},
		{"tie: the higher thread id is newer (human)", func() []map[string]any {
			return []map[string]any{
				adoWatchThread(9, adoWatchComment(1, adoWatchHumanID, "please fix", 10)),
				adoWatchThread(3, adoWatchComment(1, adoWatchSelfID, ownMarked("done"), 10)),
			}
		}, true},
		{"tie: the higher thread id is newer (Goobers)", func() []map[string]any {
			return []map[string]any{
				adoWatchThread(9, adoWatchComment(1, adoWatchSelfID, ownMarked("done"), 10)),
				adoWatchThread(3, adoWatchComment(1, adoWatchHumanID, "please fix", 10)),
			}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, fake, _ := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
				f.addPR(7, "goobers/implementation/x").threads = tc.threads()
			})
			runADOCommentWatch(t, root)
			if routed := len(fake.takeWrites()) > 0; routed != tc.routed {
				t.Fatalf("routed = %v, want %v (labels %v)", routed, tc.routed, fake.labelNames(7))
			}
		})
	}
}

// TestPRCommentWatchADOLabelRouting: an eligible PR gets goobers:needs-remediation
// through the PR-labels endpoint; a PR parked for a human has its park label
// deleted by id and is routed; excluded, draft-free foreign-head and already
// routed PRs are left alone; and a second run writes nothing (idempotent).
func TestPRCommentWatchADOLabelRouting(t *testing.T) {
	human := func() []map[string]any {
		return []map[string]any{
			adoWatchThread(1, adoWatchComment(1, adoWatchSelfID, ownMarked("verdict"), 0)),
			adoWatchThread(2, adoWatchComment(1, adoWatchHumanID, "please fix", 5)),
		}
	}
	root, fake, dir := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
		f.addPR(1, "goobers/implementation/a").threads = human()
		f.addPR(2, "goobers/implementation/b", providers.LabelNeedsHuman).threads = human()
		f.addPR(3, "goobers/implementation/c", noMergeReviewLabel).threads = human()
		f.addPR(4, "goobers/implementation/d", needsRemediationLabel).threads = human()
		f.addPR(5, "people/feature").threads = human()
	})
	stdout, _ := runADOCommentWatch(t, root)
	writes := fake.takeWrites()
	want := []string{"add:1:" + needsRemediationLabel, "remove:2:" + providers.LabelNeedsHuman, "add:2:" + needsRemediationLabel}
	if strings.Join(writes, ",") != strings.Join(want, ",") {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	if got := fake.labelNames(2); strings.Join(got, ",") != needsRemediationLabel {
		t.Fatalf("PR 2 labels = %v, want only %s after the un-park", got, needsRemediationLabel)
	}
	result := readCommentWatchResult(t, dir)
	if result.Scanned != 2 || result.Labeled != 2 || result.Unparked != 1 || result.Errors != 0 {
		t.Fatalf("result = %+v, want scanned 2 labeled 2 unparked 1", result)
	}
	if !strings.Contains(stdout, "un-parked PR #2") {
		t.Fatalf("stdout = %q, want the un-park reported", stdout)
	}

	runADOCommentWatch(t, root)
	if writes := fake.takeWrites(); len(writes) != 0 {
		t.Fatalf("second run writes = %v, want none", writes)
	}
}

// TestPRCommentWatchADOIdentityIsTheIDNotTheName: every fixture author shares
// the operator's display name. In dedicated mode an unmarked comment by the
// credential's identity id is Goobers' own, but a different identity with the
// same display name is a human.
func TestPRCommentWatchADOIdentityIsTheIDNotTheName(t *testing.T) {
	for name, tc := range map[string]struct {
		author string
		routed bool
	}{
		"same id is Goobers":            {author: adoWatchSelfID, routed: false},
		"same display name is a person": {author: adoWatchHumanID, routed: true},
	} {
		t.Run(name, func(t *testing.T) {
			root, fake, _ := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
				f.addPR(7, "goobers/implementation/x").threads = []map[string]any{
					adoWatchThread(1, adoWatchComment(1, adoWatchHumanID, "please fix", 0)),
					adoWatchThread(2, adoWatchComment(1, tc.author, "unmarked", 5)),
				}
			})
			t.Setenv(executor.InputEnvVar("identityMode"), prCommentWatchIdentityDedicated)
			runADOCommentWatch(t, root)
			if routed := len(fake.takeWrites()) > 0; routed != tc.routed {
				t.Fatalf("routed = %v, want %v", routed, tc.routed)
			}
		})
	}
}

// TestPRCommentWatchADOExcludeAuthorsByID: a configured automation identity is
// matched by identity id and joins neither watermark.
func TestPRCommentWatchADOExcludeAuthorsByID(t *testing.T) {
	root, fake, _ := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
		f.addPR(7, "goobers/implementation/x").threads = []map[string]any{
			adoWatchThread(1, adoWatchComment(1, adoWatchSelfID, ownMarked("done"), 0)),
			adoWatchThread(2, adoWatchComment(1, "ci-guid", "coverage report", 5)),
		}
	})
	t.Setenv(executor.InputEnvVar("excludeAuthors"), "CI-GUID")
	runADOCommentWatch(t, root)
	if writes := fake.takeWrites(); len(writes) != 0 {
		t.Fatalf("writes = %v, want none: the automation comment is excluded", writes)
	}
}

// TestPRCommentWatchADOFailsClosedOnIncompleteComments: a comment without an
// author id makes the PR's comment set incomplete. The PR is not routed, and
// when every scanned PR is incomplete the stage fails instead of reporting a
// clean no-work result.
func TestPRCommentWatchADOFailsClosedOnIncompleteComments(t *testing.T) {
	root, fake, _ := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {
		anonymous := adoWatchComment(1, "", "who wrote this", 5)
		f.addPR(7, "goobers/implementation/x").threads = []map[string]any{adoWatchThread(1, anonymous)}
	})
	code, _, stderr := runArgs(t, "pr-comment-watch", root)
	if code != 1 || !strings.Contains(stderr, "no author id") {
		t.Fatalf("code = %d, stderr = %q; want a stage failure naming the missing author id", code, stderr)
	}
	if writes := fake.takeWrites(); len(writes) != 0 {
		t.Fatalf("writes = %v, want none", writes)
	}
}

// TestPRCommentWatchRejectsUnknownIdentityMode keeps the input closed.
func TestPRCommentWatchRejectsUnknownIdentityMode(t *testing.T) {
	root, _, _ := adoCommentWatchFixture(t, func(f *fakeADOCommentWatch) {})
	t.Setenv(executor.InputEnvVar("identityMode"), "sometimes")
	if code, _, stderr := runArgs(t, "pr-comment-watch", root); code != 1 || !strings.Contains(stderr, "invalid identityMode") {
		t.Fatalf("code = %d, stderr = %q; want an invalid identityMode error", code, stderr)
	}
}
