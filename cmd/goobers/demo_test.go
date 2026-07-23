package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

type demoDaemonWriter struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	started chan struct{}
	once    sync.Once
}

func (w *demoDaemonWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started != nil && bytes.Contains(p, []byte("daemon started")) {
		w.once.Do(func() { close(w.started) })
	}
	return w.buf.Write(p)
}

func (w *demoDaemonWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestDemoNetworkProbe(t *testing.T) {
	const marker = "demo-network-probe"
	markerIndex := -1
	for i, arg := range os.Args {
		if arg == marker {
			markerIndex = i
			break
		}
	}
	if markerIndex < 0 {
		return
	}
	if markerIndex+1 >= len(os.Args) {
		t.Fatal("network probe address missing")
	}
	conn, err := net.DialTimeout("tcp", os.Args[markerIndex+1], time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("stage process reached the network probe")
	}
	if err := os.WriteFile("curate.json", []byte(`{"provider":"mock","phase":"curate","itemID":"DEMO-1","itemTitle":"Network probe fixture"}`), 0o644); err != nil {
		t.Fatalf("write curate result: %v", err)
	}
}

func TestDemoProviderFullLoop(t *testing.T) {
	inputs := map[string]string{}
	var payload map[string]interface{}
	for _, phase := range []string{"curate", "implement", "review", "merge-preview"} {
		var err error
		payload, _, err = demoProviderPayload(phase, inputs)
		if err != nil {
			t.Fatalf("%s payload: %v", phase, err)
		}
		for key, value := range payload {
			if text, ok := value.(string); ok {
				inputs[key] = text
			}
		}
	}
	if payload["provider"] != "mock" || payload["phase"] != "merge-preview" ||
		payload["wouldMerge"] != true || payload["pullRequestURL"] != "mock://demo/pulls/1" {
		t.Fatalf("merge preview payload = %#v", payload)
	}
}

func TestDemoProviderCommandAndErrors(t *testing.T) {
	resultFile := filepath.Join(t.TempDir(), "curate.json")
	t.Setenv("GOOBERS_INPUT_RESULTFILE", resultFile)
	var stdout, stderr bytes.Buffer
	if code := runDemoProvider([]string{"curate"}, &stdout, &stderr); code != 0 {
		t.Fatalf("curate code = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "curated DEMO-1 as ready") {
		t.Fatalf("curate stdout = %q", stdout.String())
	}
	if data, err := os.ReadFile(resultFile); err != nil || !bytes.Contains(data, []byte(`"provider":"mock"`)) {
		t.Fatalf("curate result = %q, %v", data, err)
	}

	for _, test := range []struct {
		name   string
		args   []string
		inputs map[string]string
		code   int
		want   string
	}{
		{name: "missing phase", code: 2, want: "usage:"},
		{name: "unknown phase", args: []string{"publish"}, code: 2, want: "unknown demo provider phase"},
		{name: "missing handoff", args: []string{"implement"}, code: 1, want: "itemID input is required"},
		{
			name: "failed review cannot preview",
			args: []string{"merge-preview"},
			inputs: map[string]string{
				"itemID": "DEMO-1", "pullNumber": "1", "pullRequestURL": "mock://demo/pulls/1",
				"headSHA": "head", "baseSHA": "base", "verdict": "fail",
			},
			code: 1,
			want: `verdict must be pass, got "fail"`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, key := range demoProviderInputKeys {
				t.Setenv("GOOBERS_INPUT_"+strings.ToUpper(key), "")
			}
			for key, value := range test.inputs {
				t.Setenv("GOOBERS_INPUT_"+strings.ToUpper(key), value)
			}
			var out, errOut bytes.Buffer
			if code := runDemoProvider(test.args, &out, &errOut); code != test.code {
				t.Fatalf("code = %d, want %d; stderr = %q", code, test.code, errOut.String())
			}
			if !strings.Contains(errOut.String(), test.want) {
				t.Fatalf("stderr = %q, want %q", errOut.String(), test.want)
			}
		})
	}

	t.Setenv("GOOBERS_INPUT_RESULTFILE", t.TempDir())
	stdout.Reset()
	stderr.Reset()
	if code := runDemoProvider([]string{"curate"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "write typed result") {
		t.Fatalf("unwritable result: code = %d, stderr = %q", code, stderr.String())
	}
}

func TestInitDemoBannerGolden(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tour")
	var stdout, stderr bytes.Buffer
	code := runInitWithInputForOS([]string{"--demo", root}, strings.NewReader(""), &stdout, &stderr, "linux")
	if code != 0 {
		t.Fatalf("init --demo: code = %d, stderr = %q", code, stderr.String())
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`initialized instance at %s
  created  instance.yaml
  created  config
  created  gaggles
  created  scheduler
  created  telemetry.db

Demo full loop (run these from %s):
  goobers run demo    # watch curate -> implement -> review -> merge preview
  goobers trace <id>  # inspect the journal and merge-preview artifact
`, abs, abs)
	if stdout.String() != want {
		t.Fatalf("init --demo banner:\n--- got ---\n%s--- want ---\n%s", stdout.String(), want)
	}
}

func TestInitDemoRejectsUnsupportedPlatform(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tour")
	var stdout, stderr bytes.Buffer
	code := runInitWithInputForOS([]string{"--demo", root}, strings.NewReader(""), &stdout, &stderr, "windows")
	if code != 2 {
		t.Fatalf("init --demo: code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "supported only on Linux and macOS") ||
		!strings.Contains(stderr.String(), "unavailable on windows") {
		t.Fatalf("init --demo stderr = %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("init --demo stdout = %q, want empty", stdout.String())
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("unsupported demo created its target: %v", err)
	}
}

func TestDemoTourRunsOfflineThroughDaemon(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("demo requires enforced network isolation")
	}
	start := time.Now()
	root := filepath.Join(t.TempDir(), "demo")
	if code, _, stderr := runArgs(t, "init", "--demo", root); code != 0 {
		t.Fatalf("init --demo: code = %d, stderr = %q", code, stderr)
	}
	setAPIListenAddress(t, root, freeLoopbackAddress(t))

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for network probe: %v", err)
	}
	t.Cleanup(func() { _ = probe.Close() })
	workflowPath := filepath.Join(root, "config", "gaggles", "demo", "workflows", "demo.yaml")
	workflowData, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read demo workflow: %v", err)
	}
	var demo apiv1.Workflow
	if err := yaml.Unmarshal(workflowData, &demo); err != nil {
		t.Fatalf("decode demo workflow: %v", err)
	}
	testBin, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	demo.Spec.Tasks[0].Run.Command = []string{
		testBin,
		"-test.run=^TestDemoNetworkProbe$",
		"--",
		"demo-network-probe",
		probe.Addr().String(),
	}
	workflowData, err = yaml.Marshal(demo)
	if err != nil {
		t.Fatalf("encode demo workflow: %v", err)
	}
	if err := os.WriteFile(workflowPath, workflowData, 0o644); err != nil {
		t.Fatalf("write demo workflow: %v", err)
	}

	orphan := filepath.Join(root, "workcopies", "scratch", "stage-crash-orphan")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatalf("plant crash-orphaned scratch workspace: %v", err)
	}

	var cloneAttempts atomic.Int32
	previousCloneURL := repoCloneURL
	repoCloneURL = func(apiv1.RepoRef) (string, error) {
		cloneAttempts.Add(1)
		return "", errors.New("repository access disabled by demo test")
	}
	t.Cleanup(func() { repoCloneURL = previousCloneURL })

	previousSweep := delegationSweepInterval
	delegationSweepInterval = 20 * time.Millisecond
	t.Cleanup(func() { delegationSweepInterval = previousSweep })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	upStdout := &demoDaemonWriter{started: make(chan struct{})}
	upStderr := &demoDaemonWriter{}
	upDone := make(chan struct{})
	var upCode int
	go func() {
		upCode = runUpContext(ctx, []string{"--quiet", root}, upStdout, upStderr)
		close(upDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-upDone:
			if upCode != 0 {
				t.Errorf("goobers up: code = %d, stderr = %q", upCode, upStderr.String())
			}
		case <-time.After(10 * time.Second):
			t.Error("goobers up did not stop")
		}
	})

	select {
	case <-upStdout.started:
	case <-upDone:
		t.Fatalf("goobers up exited before startup: code = %d, stderr = %q", upCode, upStderr.String())
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for goobers up")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("crash-orphaned scratch workspace was not reaped: %v", err)
	}

	code, runOut, runErr := runArgs(t, "run", "demo", root)
	if code != 0 {
		t.Fatalf("goobers run demo: code = %d, stdout = %q, stderr = %q", code, runOut, runErr)
	}
	if !strings.Contains(runOut, "phase=completed") {
		t.Fatalf("run output = %q, want completed terminal phase", runOut)
	}
	for _, phase := range []string{"curate", "implement", "review", "merge-preview"} {
		if !strings.Contains(runErr, "stage "+phase+" started") ||
			!strings.Contains(runErr, "stage "+phase+" finished") {
			t.Errorf("run progress did not show %s phase:\n%s", phase, runErr)
		}
	}
	runID := runIDFromRunStdout(t, runOut)

	code, statusOut, statusErr := runArgs(t, "status", root)
	if code != 0 {
		t.Fatalf("goobers status: code = %d, stderr = %q", code, statusErr)
	}
	if !strings.Contains(statusOut, runID) || !strings.Contains(statusOut, "completed") {
		t.Errorf("status output does not show completed demo run %s:\n%s", runID, statusOut)
	}

	code, traceOut, traceErr := runArgs(t, "trace", runID, root)
	if code != 0 {
		t.Fatalf("goobers trace: code = %d, stderr = %q", code, traceErr)
	}
	for _, want := range []string{
		"phase:    completed",
		"stage=curate",
		"stage=implement",
		"stage=review",
		"gate=review-verdict verdict=pass",
		"stage=merge-preview",
		`"provider":"mock"`,
		`"mergePreview":"would squash mock pull request #1 into main"`,
	} {
		if !strings.Contains(traceOut, want) {
			t.Errorf("trace output missing %q:\n%s", want, traceOut)
		}
	}
	if strings.Contains(traceOut, "ref.touched") {
		t.Errorf("scratch-only demo recorded a repository ref:\n%s", traceOut)
	}

	if got := cloneAttempts.Load(); got != 0 {
		t.Errorf("demo attempted %d repository operation(s)", got)
	}
	allOutput := runOut + runErr + statusOut + statusErr + traceOut + traceErr + upStdout.String() + upStderr.String()
	if strings.Contains(strings.ToLower(allOutput), "credential") {
		t.Errorf("demo emitted a credential warning: %q", allOutput)
	}
	entries, err := os.ReadDir(filepath.Join(root, "workcopies", "scratch"))
	if err != nil {
		t.Fatalf("read scratch workspace root: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("scratch workspaces were not disposed: %v", entries)
	}
	if elapsed := time.Since(start); elapsed >= time.Minute {
		t.Errorf("demo tour took %s, want under one minute", elapsed)
	}
}
