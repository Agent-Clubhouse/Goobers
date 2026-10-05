package docchurn

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type gitCall struct {
	args []string
	out  string
	err  error
}

func scriptedGit(t *testing.T, calls ...gitCall) Git {
	t.Helper()
	index := 0
	return func(repo string, args ...string) (string, error) {
		t.Helper()
		if repo != "repo" {
			t.Fatalf("repo = %q, want repo", repo)
		}
		if index >= len(calls) {
			t.Fatalf("unexpected git call: %v", args)
		}
		call := calls[index]
		index++
		if !reflect.DeepEqual(args, call.args) {
			t.Fatalf("git args = %v, want %v", args, call.args)
		}
		return call.out, call.err
	}
}

func baseOptions(t *testing.T, git Git) Options {
	t.Helper()
	return Options{
		Repo:             "repo",
		WatermarkPath:    filepath.Join(t.TempDir(), "watermark.json"),
		Gaggle:           "goobers",
		Workflow:         "docs-updater",
		SinceFloor:       time.Hour,
		BufferMultiplier: 2,
		AdvanceWatermark: true,
		Clock: func() time.Time {
			return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
		},
		Git:    git,
		Stdout: &bytes.Buffer{},
	}
}

func TestRunUsesOverlapAndParsesGitResponses(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	refreshedAt := now.Add(-3 * time.Hour)
	path := filepath.Join(t.TempDir(), "watermark.json")
	if err := writeWatermark(path, docsWatermark{
		Schema: watermarkSchemaVersion, Gaggle: "goobers", Workflow: "docs-updater",
		SHA: "old", RefreshedAt: refreshedAt,
	}); err != nil {
		t.Fatal(err)
	}
	resultFile := filepath.Join(t.TempDir(), "docs-churn.json")
	git := scriptedGit(t,
		gitCall{args: []string{"rev-parse", "--verify", "HEAD^{commit}"}, out: "head\n"},
		gitCall{args: []string{"rev-list", "-1", "--before=2026-10-05T03:00:00Z", "HEAD"}, out: "base\n"},
		gitCall{
			args: []string{"log", "-z", "--no-color", "--format=%H\x1e%s\x1e%b", "base..head"},
			out:  "head\x1esubject\x1ebody\x00",
		},
		gitCall{args: []string{"diff", "--no-renames", "--name-only", "base", "head"}, out: "src/x.go\ndocs/guide.md\n"},
	)
	opts := baseOptions(t, git)
	opts.WatermarkPath = path
	opts.ResultFile = resultFile
	opts.DocsRoots = []string{"docs"}
	if err := Run(opts); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(resultFile)
	if err != nil {
		t.Fatal(err)
	}
	var digest docsChurnDigest
	if err := json.Unmarshal(data, &digest); err != nil {
		t.Fatal(err)
	}
	if !digest.Since.Equal(time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC)) {
		t.Errorf("since = %v, want overlap edge at 03:00", digest.Since)
	}
	if digest.FirstRun || digest.Base != "base" || digest.Head != "head" {
		t.Errorf("digest window = %+v", digest)
	}
	if len(digest.Commits) != 1 || digest.Commits[0] != (churnCommit{SHA: "head", Subject: "subject", Body: "body"}) {
		t.Errorf("commits = %+v", digest.Commits)
	}
	if got, want := digest.ChangedFiles, []string{"docs/guide.md", "src/x.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("changed files = %v, want %v", got, want)
	}
	if got, want := digest.DocsRootChanges, []string{"docs/guide.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("docs root changes = %v, want %v", got, want)
	}
	watermark, have, err := readWatermark(path)
	if err != nil || !have {
		t.Fatalf("read watermark: have=%v err=%v", have, err)
	}
	if watermark.SHA != "head" || !watermark.RefreshedAt.Equal(now) {
		t.Errorf("watermark = %+v", watermark)
	}
}

func TestRunReportsEveryGitFailureWithoutAdvancingWatermark(t *testing.T) {
	tests := []struct {
		name  string
		calls []gitCall
		want  string
	}{
		{
			name:  "resolve head",
			calls: []gitCall{{args: []string{"rev-parse", "--verify", "HEAD^{commit}"}, err: context.Canceled}},
			want:  "resolve HEAD in repo: context canceled",
		},
		{
			name: "boundary",
			calls: []gitCall{
				{args: []string{"rev-parse", "--verify", "HEAD^{commit}"}, out: "head"},
				{args: []string{"rev-list", "-1", "--before=2026-10-05T11:00:00Z", "HEAD"}, err: errors.New("boundary failed")},
			},
			want: "locate window boundary commit in repo: boundary failed",
		},
		{
			name: "commits",
			calls: []gitCall{
				{args: []string{"rev-parse", "--verify", "HEAD^{commit}"}, out: "head"},
				{args: []string{"rev-list", "-1", "--before=2026-10-05T11:00:00Z", "HEAD"}, out: "base"},
				{args: []string{"log", "-z", "--no-color", "--format=%H\x1e%s\x1e%b", "base..head"}, err: errors.New("log failed")},
			},
			want: "list commits in repo: log failed",
		},
		{
			name: "changed files",
			calls: []gitCall{
				{args: []string{"rev-parse", "--verify", "HEAD^{commit}"}, out: "head"},
				{args: []string{"rev-list", "-1", "--before=2026-10-05T11:00:00Z", "HEAD"}, out: "base"},
				{args: []string{"log", "-z", "--no-color", "--format=%H\x1e%s\x1e%b", "base..head"}},
				{args: []string{"diff", "--no-renames", "--name-only", "base", "head"}, err: errors.New("diff failed")},
			},
			want: "list changed files in repo: diff failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := baseOptions(t, scriptedGit(t, tt.calls...))
			err := Run(opts)
			if err == nil || err.Error() != tt.want {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
			if tt.name == "resolve head" && !errors.Is(err, context.Canceled) {
				t.Errorf("cancellation was not preserved: %v", err)
			}
			if _, err := os.Stat(opts.WatermarkPath); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("watermark exists after Git failure: %v", err)
			}
		})
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed output") }

func successfulGit(t *testing.T) Git {
	t.Helper()
	return scriptedGit(t,
		gitCall{args: []string{"rev-parse", "--verify", "HEAD^{commit}"}, out: "head"},
		gitCall{args: []string{"rev-list", "-1", "--before=2026-10-05T11:00:00Z", "HEAD"}},
		gitCall{args: []string{"log", "-z", "--no-color", "--format=%H\x1e%s\x1e%b", "head"}},
		gitCall{args: []string{"diff", "--no-renames", "--name-only", emptyTreeObject, "head"}},
	)
}

func TestRunOutputFailureDoesNotAdvanceWatermark(t *testing.T) {
	opts := baseOptions(t, successfulGit(t))
	opts.Stdout = failWriter{}
	err := Run(opts)
	var outputErr *StdoutError
	if !errors.As(err, &outputErr) || err.Error() != "closed output" {
		t.Fatalf("error = %v, want StdoutError", err)
	}
	if _, err := os.Stat(opts.WatermarkPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("watermark exists after output failure: %v", err)
	}
}

func TestRunPersistenceFailureFollowsSuccessfulOutput(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	opts := baseOptions(t, successfulGit(t))
	opts.WatermarkPath = filepath.Join(blocker, "watermark.json")
	opts.ResultFile = blocker
	err := Run(opts)
	if err == nil || !strings.HasPrefix(err.Error(), "advance docs watermark ") {
		t.Fatalf("error = %v, want watermark persistence failure", err)
	}
	output, readErr := os.ReadFile(blocker)
	if readErr != nil || len(output) == 0 || output[len(output)-1] != '\n' {
		t.Errorf("digest was not fully written before persistence failure: %q, err=%v", output, readErr)
	}
}

func TestWriteDigestExactStdout(t *testing.T) {
	digest := docsChurnDigest{
		Schema:           schemaVersion,
		FirstRun:         true,
		Since:            time.Date(2026, 10, 4, 1, 2, 3, 0, time.UTC),
		Head:             "head-sha",
		BufferMultiplier: 3,
		SinceFloor:       "168h0m0s",
		CommitCount:      1,
		Commits:          []churnCommit{{SHA: "commit-sha", Subject: "subject", Body: "body"}},
		ChangedFiles:     []string{"docs/guide.md"},
		Areas:            map[string][]string{"docs": {"docs/guide.md"}},
		DocsRoots:        []string{"docs"},
		DocsRootChanges:  []string{"docs/guide.md"},
		Note:             firstRunNote,
	}
	var stdout bytes.Buffer
	if err := writeDigest(digest, "", &stdout); err != nil {
		t.Fatal(err)
	}
	const want = "{\n" +
		"  \"schema\": \"goobers.dev/docs-churn/v1\",\n" +
		"  \"firstRun\": true,\n" +
		"  \"since\": \"2026-10-04T01:02:03Z\",\n" +
		"  \"head\": \"head-sha\",\n" +
		"  \"bufferMultiplier\": 3,\n" +
		"  \"sinceFloor\": \"168h0m0s\",\n" +
		"  \"commitCount\": 1,\n" +
		"  \"commits\": [\n" +
		"    {\n" +
		"      \"sha\": \"commit-sha\",\n" +
		"      \"subject\": \"subject\",\n" +
		"      \"body\": \"body\"\n" +
		"    }\n" +
		"  ],\n" +
		"  \"changedFiles\": [\n" +
		"    \"docs/guide.md\"\n" +
		"  ],\n" +
		"  \"areas\": {\n" +
		"    \"docs\": [\n" +
		"      \"docs/guide.md\"\n" +
		"    ]\n" +
		"  },\n" +
		"  \"docsRoots\": [\n" +
		"    \"docs\"\n" +
		"  ],\n" +
		"  \"docsRootChanges\": [\n" +
		"    \"docs/guide.md\"\n" +
		"  ],\n" +
		"  \"note\": \"first run: no watermark yet, bounded to the since-floor window\"\n" +
		"}\n"
	if stdout.String() != want {
		t.Errorf("stdout bytes =\n%s\nwant exact bytes =\n%s", stdout.String(), want)
	}
}

func TestWriteWatermarkExactAtomicContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler", "docs-updater", "goobers___docs-updater.json")
	watermark := docsWatermark{
		Schema: watermarkSchemaVersion, Gaggle: "goobers", Workflow: "docs-updater",
		SHA: "0123456789abcdef", RefreshedAt: time.Date(2026, 10, 4, 1, 2, 3, 0, time.UTC),
	}
	if err := writeWatermark(path, watermark); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const want = "{\n" +
		"  \"schema\": \"goobers.dev/docs-watermark/v1\",\n" +
		"  \"gaggle\": \"goobers\",\n" +
		"  \"workflow\": \"docs-updater\",\n" +
		"  \"sha\": \"0123456789abcdef\",\n" +
		"  \"refreshedAt\": \"2026-10-04T01:02:03Z\"\n" +
		"}\n"
	if string(data) != want {
		t.Errorf("watermark bytes =\n%s\nwant exact bytes =\n%s", data, want)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("temporary watermark remains after replace: %v", err)
	}
}

func TestRunResultFileFailureDoesNotAdvanceWatermark(t *testing.T) {
	opts := baseOptions(t, successfulGit(t))
	opts.ResultFile = t.TempDir()
	err := Run(opts)
	if err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("write result file %q:", opts.ResultFile)) {
		t.Fatalf("error = %v, want result file failure", err)
	}
	if _, err := os.Stat(opts.WatermarkPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("watermark exists after result-file failure: %v", err)
	}
}
