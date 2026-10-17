//go:build integration

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// This integration fixture executes this already-built test binary, not an
// external tool: no Go/Git/cloud executable or network service is required.
func TestIntegrationVerdictCommitAcrossProcesses(t *testing.T) {
	root := newVerdictTestRoot(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	type writer struct {
		cmd   *exec.Cmd
		input io.WriteCloser
		out   *bufio.Reader
	}
	start := func(role string) writer {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestIntegrationVerdictCommitProcessHelper$")
		cmd.Env = append(os.Environ(), "GOOBERS_TEST_VERDICT_PROCESS="+role, "GOOBERS_TEST_VERDICT_ROOT="+root)
		cmd.Stderr = os.Stderr
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = input.Close() })
		reader := bufio.NewReader(output)
		if ready, err := reader.ReadString('\n'); err != nil || ready != "READY\n" {
			t.Fatalf("helper %s did not capture predecessor: %q %v", role, ready, err)
		}
		return writer{cmd, input, reader}
	}
	finish := func(w writer) apiv1.Verdict {
		t.Helper()
		if _, err := io.WriteString(w.input, "commit\n"); err != nil {
			t.Fatal(err)
		}
		line, err := w.out.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		var verdict apiv1.Verdict
		if err := json.Unmarshal([]byte(line), &verdict); err != nil {
			t.Fatalf("helper result %q: %v", line, err)
		}
		if err := w.cmd.Wait(); err != nil {
			t.Fatalf("helper failed: %v", err)
		}
		return verdict
	}
	// Both processes capture the absent predecessor before either commits.
	pass, failure := start("pass"), start("failure")
	failed := finish(failure)
	got := finish(pass)
	if failed.LedgerGeneration != 1 || got.LedgerGeneration != 2 || got.Decision != apiv1.VerdictNeedsChanges || len(got.StandingFindings) != 1 {
		t.Fatalf("two-process stale pass erased the finding: failure=%+v pass=%+v", failed, got)
	}
	stored, ok, err := loadVerdictState(root, verdictCommitRepo, 55)
	if err != nil || !ok || stored.Decision != got.Decision || stored.LedgerGeneration != got.LedgerGeneration {
		t.Fatalf("fresh reader disagrees with the two writers: %+v %v", stored, err)
	}
}

func TestIntegrationVerdictCommitProcessHelper(t *testing.T) {
	role := os.Getenv("GOOBERS_TEST_VERDICT_PROCESS")
	if role == "" {
		return
	}
	publication, err := beginVerdictPublication(os.Getenv("GOOBERS_TEST_VERDICT_ROOT"), verdictCommitRepo, 55)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("READY")
	if line, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil || strings.TrimSpace(line) != "commit" {
		t.Fatalf("commit barrier: %q %v", line, err)
	}
	candidate := apiv1.Verdict{Decision: apiv1.VerdictPass, HeadSHA: "head", BaseSHA: "base"}
	if role == "failure" {
		candidate.Decision = apiv1.VerdictNeedsChanges
		candidate.Findings = []apiv1.Finding{unboundedLineageFinding()}
	}
	committed, err := publication.commit(candidate, nil, nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(committed); err != nil {
		t.Fatal(err)
	}
}
