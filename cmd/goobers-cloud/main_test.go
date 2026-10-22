package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/workerhost"
)

func runForTest(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestHelpGolden(t *testing.T) {
	code, stdout, stderr := runForTest("--help")
	if code != 0 || stderr != "" {
		t.Fatalf("--help: code=%d stderr=%q", code, stderr)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "help.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if stdout != string(want) {
		t.Fatalf("--help drifted:\n%s", stdout)
	}
	for _, role := range []string{"api", "controller", "worker", "migrate"} {
		if !strings.Contains(stdout, role) {
			t.Errorf("--help omits role %q", role)
		}
	}
}

func TestWorkerUsesWorkerHost(t *testing.T) {
	originalNew, originalRun, originalSignal := newWorkerHost, runWorkerHost, signalContext
	t.Cleanup(func() {
		newWorkerHost, runWorkerHost, signalContext = originalNew, originalRun, originalSignal
	})
	var got workerhost.Config
	newWorkerHost = func(cfg workerhost.Config) (*workerhost.Host, error) {
		got = cfg
		return &workerhost.Host{}, nil
	}
	runWorkerHost = func(context.Context, *workerhost.Host) error { return nil }
	signalContext = func() (context.Context, func()) { return context.Background(), func() {} }

	code, stdout, stderr := runForTest("worker", "--task-queue", "one", "--task-queue", "two", "--health-addr", ":8080")
	if code != 0 || stderr != "" {
		t.Fatalf("worker: code=%d stderr=%q", code, stderr)
	}
	if strings.Join(got.TaskQueues, ",") != "one,two" || got.HealthAddr != ":8080" {
		t.Fatalf("worker host config = %+v", got)
	}
	if !strings.Contains(stdout, "drained cleanly") {
		t.Fatalf("worker output = %q", stdout)
	}
}

func TestWorkerMapsHostErrors(t *testing.T) {
	originalNew, originalRun, originalSignal := newWorkerHost, runWorkerHost, signalContext
	t.Cleanup(func() {
		newWorkerHost, runWorkerHost, signalContext = originalNew, originalRun, originalSignal
	})
	newWorkerHost = func(workerhost.Config) (*workerhost.Host, error) { return &workerhost.Host{}, nil }
	runWorkerHost = func(context.Context, *workerhost.Host) error { return workerhost.ErrAbandonedWork }
	signalContext = func() (context.Context, func()) { return context.Background(), func() {} }

	code, _, stderr := runForTest("worker")
	if code != 3 || !strings.Contains(stderr, "drain timeout") {
		t.Fatalf("worker abandoned: code=%d stderr=%q", code, stderr)
	}
}

func TestGeneratedDocs(t *testing.T) {
	root := t.TempDir()
	if err := writeDocs(root); err != nil {
		t.Fatal(err)
	}
	for relative, want := range renderDocs() {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		if string(got) != want {
			t.Errorf("%s differs from rendered docs", relative)
		}
	}
	if _, _, stderr := runForTest("__generate-docs"); !strings.Contains(stderr, "usage:") {
		t.Fatalf("missing generator usage: %q", stderr)
	}
}

func TestCommittedDocsUpToDate(t *testing.T) {
	root := filepath.Join("..", "..", "docs")
	for relative, want := range renderDocs() {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(relative)))
		if err != nil {
			t.Fatalf("read %s: %v; regenerate with `make docs`", relative, err)
		}
		if string(got) != want {
			t.Fatalf("%s is stale; regenerate with `make docs`", relative)
		}
	}
}

func TestCompletionCommand(t *testing.T) {
	code, stdout, stderr := runForTest("completion", "bash")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "goobers-cloud") {
		t.Fatalf("completion bash: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}
