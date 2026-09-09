package journal

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const transcriptCrashRoot = "GOOBERS_TRANSCRIPT_CRASH_ROOT"

func TestTranscriptCheckpointSurvivesAbruptProcessKill(t *testing.T) {
	if root := os.Getenv(transcriptCrashRoot); root != "" {
		runTranscriptCrashChild(t, root)
		return
	}
	t.Setenv(envDisableFsync, "0")
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTranscriptCheckpointSurvivesAbruptProcessKill$")
	cmd.Env = append(os.Environ(), transcriptCrashRoot+"="+root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- line
	}()
	select {
	case line := <-ready:
		if line != "checkpoint-ready\n" {
			t.Fatal("child did not acknowledge a durable checkpoint")
		}
	case <-ctx.Done():
		t.Fatal("child checkpoint timed out")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("child exited normally instead of being killed")
	}
	directory := filepath.Join(root, testIdentity().RunID)
	recovered, report, err := Recover(directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(directory)
	if err != nil {
		t.Fatal(err)
	}
	var content []byte
	count := 0
	for _, event := range report.Events {
		if event.Type != EventSpanRecorded || event.Runner["partial"] != true {
			continue
		}
		data, err := reader.SpanBytes(*event.Ref)
		if err != nil {
			t.Fatal(err)
		}
		content = append(content, data...)
		count++
	}
	if count != 2 || string(content) != "captured before kill: "+Redacted+"\n" {
		t.Fatalf("abrupt kill lost or exposed checkpoint bytes: chunks=%d", count)
	}
}

func runTranscriptCrashChild(t *testing.T, root string) {
	t.Helper()
	run, err := Create(root, testIdentity(), nil, WithScrubber(NewPatternScrubber()))
	if err != nil {
		t.Fatal(err)
	}
	capture, err := run.BeginTranscriptCapture("implement", "transcript")
	if err != nil {
		t.Fatal(err)
	}
	parts := []string{"captured before kill: ghp_", strings.Repeat("A", 80) + "\n"}
	offset := 0
	for _, part := range parts {
		if err := capture.Append(TranscriptCheckpoint{Stream: "process-output/1", Offset: offset,
			Data: []byte(part), Reason: "checkpoint"}); err != nil {
			t.Fatal(err)
		}
		offset += len(part)
	}
	_, _ = fmt.Fprintln(os.Stdout, "checkpoint-ready")
	// No Close or final transcript: the parent terminates this process after
	// its acknowledgment. A live timer prevents Go's deadlock detector exit.
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	<-timer.C
	t.Fatal("parent did not kill the checkpoint child")
}
