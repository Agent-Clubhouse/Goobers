//go:build !windows

package harness

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func TestPeriodicTranscriptSurvivesSupervisorSignals(t *testing.T) {
	if os.Getenv("GOOBERS_SIGNAL_TRANSCRIPT_ROLE") == "producer" {
		fmt.Printf("pid=%d\nworking before signal\n", os.Getpid())
		time.Sleep(20 * time.Second)
		return
	}
	if os.Getenv("GOOBERS_SIGNAL_TRANSCRIPT_ROLE") == "supervisor" {
		runSignalTranscriptSupervisor(t)
		return
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		t.Run(sig.String(), func(t *testing.T) {
			root := t.TempDir()
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPeriodicTranscriptSurvivesSupervisorSignals$")
			cmd.Env = append(os.Environ(), "GOOBERS_SIGNAL_TRANSCRIPT_ROLE=supervisor", "GOOBERS_SIGNAL_TRANSCRIPT_ROOT="+root, "GOOBERS_DISABLE_FSYNC=0")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
			ready := make(chan string, 1)
			go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
			select {
			case line := <-ready:
				var pid int
				if _, err := fmt.Sscanf(line, "ready %d", &pid); err != nil || pid <= 0 {
					t.Fatalf("supervisor did not checkpoint: %q", line)
				}
				// SIGKILL cannot run the supervisor's process-group cleanup.
				producer, err := os.FindProcess(pid)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = producer.Kill() })
			case <-ctx.Done():
				t.Fatal("periodic checkpoint was not acknowledged before timeout")
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			err = cmd.Wait()
			if sig == syscall.SIGTERM && err != nil {
				t.Fatalf("graceful supervisor exit: %v", err)
			}
			if sig == syscall.SIGKILL && err == nil {
				t.Fatal("SIGKILL supervisor exited normally")
			}
			dir := filepath.Join(root, "signal-run")
			recovered, report, err := journal.Recover(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := recovered.Close(); err != nil {
				t.Fatal(err)
			}
			reader, err := journal.OpenReadOnly(dir)
			if err != nil {
				t.Fatal(err)
			}
			var content []byte
			canceled := false
			for _, event := range report.Events {
				if event.Runner["partial"] != true {
					continue
				}
				data, err := reader.SpanBytes(*event.Ref)
				if err != nil {
					t.Fatal(err)
				}
				content = append(content, data...)
				canceled = canceled || event.Runner["reason"] == "canceled"
			}
			if !strings.Contains(string(content), "working before signal\n") {
				t.Fatalf("signal lost periodic output: %q", content)
			}
			if sig == syscall.SIGTERM && !canceled {
				t.Fatal("SIGTERM lost final cancellation reason")
			}
		})
	}
}

func runSignalTranscriptSupervisor(t *testing.T) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	run, err := journal.Create(os.Getenv("GOOBERS_SIGNAL_TRANSCRIPT_ROOT"), journal.RunIdentity{
		RunID: "signal-run", Gaggle: "web", Workflow: "test", WorkflowVersion: 1,
		WorkflowDigest: "sha256:abc", Trigger: journal.Trigger{Kind: journal.TriggerManual},
	}, nil, journal.WithScrubber(journal.NewPatternScrubber()))
	if err != nil {
		t.Fatal(err)
	}
	capture, err := run.BeginTranscriptCapture("build", "copilot-cli.transcript")
	if err != nil {
		t.Fatal(err)
	}
	var observed []byte
	ready := false
	_, err = (ExecProcessRunner{}).Run(ctx, ProcessRequest{
		Command: []string{os.Args[0], "-test.run=^TestPeriodicTranscriptSurvivesSupervisorSignals$"},
		Env:     []string{"GOOBERS_SIGNAL_TRANSCRIPT_ROLE=producer"}, Timeout: 10 * time.Second,
		TranscriptCheckpointInterval: 20 * time.Millisecond,
		TranscriptCheckpoint: func(delta TranscriptDelta) error {
			if err := capture.Append(journal.TranscriptCheckpoint{Stream: "process-output/1", Offset: delta.Offset,
				Data: delta.Data, DroppedBytes: delta.DroppedBytes, Reason: delta.Reason}); err != nil {
				return err
			}
			observed = append(observed, delta.Data...)
			if !ready && strings.Contains(string(observed), "working before signal\n") {
				var pid int
				if _, err := fmt.Sscanf(string(observed), "pid=%d", &pid); err != nil {
					return err
				}
				ready = true
				_, err := fmt.Fprintf(os.Stdout, "ready %d\n", pid)
				return err
			}
			return nil
		},
	})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("supervisor did not cancel harness: %v", err)
	}
	if err := run.Close(); err != nil {
		t.Fatal(err)
	}
}
