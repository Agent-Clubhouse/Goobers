package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/chat"
	"github.com/goobers/goobers/internal/chat/chattest"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/platform/proc"
)

type fakeReport struct {
	Complete bool
	Error    string
	Frames   []object
	Args     []string
	Env      []string
}

// TestClaudeFakeCLI is a real Go subprocess that replays the upstream fixtures.
// No shell, credentials, network, installed CLI, or model calls are involved.
func TestClaudeFakeCLI(t *testing.T) {
	marker := slices.Index(os.Args, "--claude-fake")
	if marker < 0 {
		return
	}
	mode, reportPath := os.Args[marker+1], os.Args[marker+2]
	if mode == "child-writer" {
		for i := 0; ; i++ {
			if err := os.WriteFile(reportPath, []byte(fmt.Sprint(i)), 0600); err != nil {
				os.Exit(4)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	report := fakeReport{Args: os.Args[marker+3:], Env: os.Environ()}
	save := func() {
		data, _ := json.Marshal(report)
		if err := os.WriteFile(reportPath, data, 0600); err != nil {
			os.Exit(4)
		}
	}
	decoder := json.NewDecoder(bufio.NewReader(os.Stdin))
	encoder := json.NewEncoder(os.Stdout)
	receive := func() (object, error) {
		var m object
		err := decoder.Decode(&m)
		if err == nil {
			report.Frames = append(report.Frames, m)
			save()
		}
		return m, err
	}
	if mode == "controls" || mode == "stall" {
		for {
			m, err := receive()
			if err != nil {
				os.Exit(0)
			}
			if mode == "stall" {
				continue
			}
			if m["type"] == "control_request" {
				_ = encoder.Encode(object{"type": "control_response", "response": object{"subtype": "success", "request_id": m["request_id"], "response": object{"claude_code_version": "fixture"}}})
			}
			if m["type"] == "user" {
				switch textContent(obj(m["message"])["content"]) {
				case "child":
					child := exec.Command(os.Args[0], "-test.run=^TestClaudeFakeCLI$", "--", "--claude-fake", "child-writer", reportPath+".writer")
					child.Env = []string{}
					if err := child.Start(); err != nil {
						os.Exit(4)
					}
					go func() { _ = child.Wait() }()
					_ = encoder.Encode(object{"type": "assistant", "message": object{"content": []object{{"type": "text", "text": fmt.Sprint(child.Process.Pid)}}}})
				case "finish-queued":
					_ = encoder.Encode(object{"type": "result", "subtype": "success", "queued_turn_count": 1})
					_ = encoder.Encode(object{"type": "assistant", "message": object{"content": []object{{"type": "text", "text": "queued reply"}}}})
					_ = encoder.Encode(object{"type": "result", "subtype": "success", "queued_turn_count": 0})
				case "permission":
					_ = encoder.Encode(object{"type": "control_request", "request_id": "allow-1", "request": object{"subtype": "can_use_tool", "tool_name": "Edit", "input": object{"file_path": "file.txt", "new_string": "text"}}})
				case "cancel":
					_ = encoder.Encode(object{"type": "control_cancel_request", "request_id": "allow-1"})
				case "overflow":
					for range 1100 {
						_ = encoder.Encode(object{"type": "future_frame"})
					}
				}
			}
		}
	}
	f, err := os.Open(mode)
	if err != nil {
		os.Exit(4)
	}
	fixture, err := chattest.ReadFixture(f)
	_ = f.Close()
	if err != nil {
		os.Exit(4)
	}
	err = fixture.Replay(receive, func(e chattest.Entry) error {
		switch {
		case e.Msg != nil:
			return encoder.Encode(e.Msg)
		case e.Raw != nil:
			_, err := fmt.Fprintln(os.Stdout, *e.Raw)
			return err
		case e.Stderr != nil:
			return nil
		case e.Exit != nil:
			os.Exit(e.Exit.Code)
		}
		return nil
	})
	if err != nil {
		report.Error = err.Error()
		save()
		os.Exit(3)
	}
	report.Complete = true
	save()
	for {
		_, err := receive()
		if err != nil {
			os.Exit(0)
		}
		report.Error = "unexpected extra client frame"
		save()
		os.Exit(3)
	}
}
func fakeDriver(t *testing.T, mode string) (*Driver, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("process-tree custody proof is not yet implemented on Windows")
	}
	path := mode
	if mode != "controls" && mode != "stall" {
		var err error
		path, err = filepath.Abs(filepath.Join("testdata", mode+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
	}
	report := filepath.Join(t.TempDir(), "report.json")
	d := &Driver{ControlTimeout: 2 * time.Second, Prepare: func(_ context.Context, _ chat.StartOptions, args []string) (Launch, error) {
		command := append([]string{os.Args[0], "-test.run=^TestClaudeFakeCLI$", "--", "--claude-fake", path, report}, args...)
		return Launch{Command: command, Env: []string{"CLAUDECODE=host", "CLAUDE_CODE_ENTRYPOINT=host", "CLAUDE_CODE_CHILD_SESSION=host"}}, nil
	}}
	return d, report
}
func waitEvent(t *testing.T, s chat.Session, kind string) chat.Event {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case e, ok := <-s.Events():
			if !ok {
				t.Fatalf("closed before %s", kind)
			}
			if e.Type == kind {
				return e
			}
		case <-deadline.C:
			t.Fatalf("waiting for %s", kind)
		}
	}
}
func readReport(t *testing.T, path string) fakeReport {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		var report fakeReport
		if err == nil && json.Unmarshal(data, &report) == nil {
			return report
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("missing fake CLI report")
	return fakeReport{}
}
func TestDriverConformance(t *testing.T) {
	chattest.Conformance(t, func() chat.Driver { d, _ := fakeDriver(t, "controls"); return d })
}
func TestDriverFixtureReplay(t *testing.T) {
	paths, _ := filepath.Glob("testdata/*.jsonl")
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		t.Run(name, func(t *testing.T) {
			d, reportPath := fakeDriver(t, name)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ctx, proof := invoke.WithWorkspaceQuiescence(ctx)
			s, err := d.Start(ctx, chat.StartOptions{SessionID: "session", Fence: 9, Ceiling: chat.NewCeiling(chat.Ask, chat.Ask, chat.Ask), Level: chat.Ask})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.NativeID() != "" {
				t.Fatal("native session appeared before first prompt")
			}
			send := func() {
				if _, err := s.Send(ctx, chat.Text("fixture prompt"), chat.SendOptions{CommandID: "command"}); err != nil {
					t.Fatal(err)
				}
			}
			send()
			turns := 1
			if name == "multi-turn" || name == "turn-error" || name == "session-info" {
				turns = 2
			}
			counts := map[string]int{}
			var seq uint64
			for completed := 0; completed < turns; {
				select {
				case e, ok := <-s.Events():
					if !ok {
						t.Fatalf("early close: %+v", readReport(t, reportPath))
					}
					if e.Seq <= seq || e.Fence != 9 {
						t.Fatal("bad envelope")
					}
					seq = e.Seq
					counts[e.Type]++
					switch e.Type {
					case "tool_call":
						if name == "interrupt" {
							if err := s.Interrupt(ctx); err != nil {
								t.Fatal(err)
							}
						}
					case "permission_request":
						option := "allow_once"
						if name == "permission-deny" {
							option = "deny"
						}
						if err := s.RespondPermission(ctx, str(e.Data["requestId"]), chat.PermissionResponse{OptionID: option}); err != nil {
							t.Fatal(err)
						}
					case "question":
						if err := s.AnswerQuestion(ctx, str(e.Data["requestId"]), chat.QuestionAnswers{"Which linter should I set up?": {"ESLint"}, "Which rule sets do you want?": {"Recommended", "TypeScript"}}); err != nil {
							t.Fatal(err)
						}
					case "turn_ended":
						completed++
						if !errors.Is(proof.Verify(), invoke.ErrWorkspaceNotQuiescent) {
							t.Fatal("result incorrectly released custody")
						}
						if name == "interrupt" && e.Data["reason"] != "cancelled" {
							t.Fatal(e.Data)
						}
						if completed < turns {
							if name == "session-info" {
								if err := s.SetModel(ctx, "sonnet"); err != nil {
									t.Fatal(err)
								}
							}
							send()
						}
					}
				case <-ctx.Done():
					t.Fatalf("fixture timeout: %+v", readReport(t, reportPath))
				}
			}
			// The fake marks completion immediately after emitting the final frame.
			deadline := time.Now().Add(time.Second)
			report := readReport(t, reportPath)
			for !report.Complete && report.Error == "" && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
				report = readReport(t, reportPath)
			}
			if !report.Complete || report.Error != "" {
				t.Fatalf("incomplete replay: %+v", report)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := proof.Verify(); err != nil {
				t.Fatal(err)
			}
			if name == "turn-error" && counts["error"] != 1 {
				t.Fatal(counts)
			}
			if name == "schema-drift" && counts["protocol_unknown"] < 2 {
				t.Fatal(counts)
			}
			if name == "permission-approve-edit" {
				var echoed object
				for _, m := range report.Frames {
					if m["type"] == "control_response" {
						echoed = obj(obj(obj(m["response"])["response"])["updatedInput"])
					}
				}
				if echoed["file_path"] != "/workspace/project/src/greet.ts" {
					t.Fatalf("allow failed to echo original input: %v", echoed)
				}
			}
		})
	}
}
func TestNativeControlsAndPriorities(t *testing.T) {
	d, reportPath := fakeDriver(t, "controls")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := d.Start(ctx, chat.StartOptions{SystemPrompt: "private system prompt", Level: chat.Ask, Ceiling: chat.NewCeiling(chat.AutoEdit, chat.FullAccess, chat.FullAccess)})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, delivery := range []chat.Delivery{chat.Prompt, chat.Steer, chat.Queued} {
		if got, err := s.Send(ctx, chat.Text("hold"), chat.SendOptions{Delivery: delivery}); err != nil || got != delivery {
			t.Fatal(got, err)
		}
	}
	if _, err := s.Send(ctx, chat.Text("busy"), chat.SendOptions{}); !errors.Is(err, chat.ErrBusy) {
		t.Fatal(err)
	}
	if level, err := s.SetLevel(ctx, chat.FullAccess); err != nil || level != chat.AutoEdit {
		t.Fatal(level, err)
	}
	if err := s.SetModel(ctx, "haiku"); err != nil {
		t.Fatal(err)
	}
	if err := s.(*Session).SetEffort(ctx, "low"); err != nil {
		t.Fatal(err)
	}
	report := readReport(t, reportPath)
	if strings.Contains(strings.Join(report.Args, " "), "private system prompt") {
		t.Fatal("prompt in argv")
	}
	for _, env := range report.Env {
		if strings.HasPrefix(env, "CLAUDECODE=") || strings.HasPrefix(env, "CLAUDE_CODE_ENTRYPOINT=") || strings.HasPrefix(env, "CLAUDE_CODE_CHILD_SESSION=") {
			t.Fatal("host session leaked")
		}
	}
	var priorities []string
	for _, m := range report.Frames {
		if m["type"] == "user" {
			priorities = append(priorities, str(m["priority"]))
		}
	}
	if !slices.Equal(priorities, []string{"", "next", "later"}) {
		t.Fatal(priorities)
	}
}
func TestPermissionCancellationAndOverflow(t *testing.T) {
	d, _ := fakeDriver(t, "controls")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	session, err := d.Start(ctx, chat.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.Send(ctx, chat.Text("permission"), chat.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, session, "permission_request")
	if _, err := session.Send(ctx, chat.Text("cancel"), chat.SendOptions{Delivery: chat.Steer}); err != nil {
		t.Fatal(err)
	}
	waitEvent(t, session, "permission_resolved")
	if err := session.RespondPermission(ctx, "allow-1", chat.PermissionResponse{OptionID: "allow_once"}); !errors.Is(err, chat.ErrUnknownRequest) {
		t.Fatal(err)
	}
	if _, err := session.Send(ctx, chat.Text("overflow"), chat.SendOptions{Delivery: chat.Steer}); err != nil && !errors.Is(err, chat.ErrClosed) {
		t.Fatal(err)
	}
	select {
	case <-session.(*Session).done:
	case <-ctx.Done():
		t.Fatal("overflow did not stop writer")
	}
}
func TestHandshakeTimeoutJoinsProcess(t *testing.T) {
	d, _ := fakeDriver(t, "stall")
	d.ControlTimeout = 100 * time.Millisecond
	ctx, proof := invoke.WithWorkspaceQuiescence(context.Background())
	_, err := d.Start(ctx, chat.StartOptions{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := proof.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestCloseJoinsDescendantWriter(t *testing.T) {
	d, report := fakeDriver(t, "controls")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := d.Start(ctx, chat.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Send(ctx, chat.Text("child"), chat.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	e := waitEvent(t, s, "agent_message")
	blocks := e.Data["content"].([]chat.ContentBlock)
	var pid int
	if _, err := fmt.Sscan(blocks[0].Text, &pid); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(report + ".writer"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("descendant never wrote")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if proc.Alive(pid) {
		t.Fatal("descendant remains alive after custody release")
	}
}
func TestQueuedTurnLifecycle(t *testing.T) {
	d, _ := fakeDriver(t, "controls")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := d.Start(ctx, chat.StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if actual, err := s.Send(ctx, chat.Text("hold"), chat.SendOptions{Delivery: chat.Queued}); err != nil || actual != chat.Prompt {
		t.Fatal("idle delivery", actual, err)
	}
	if _, err := s.Send(ctx, chat.Text("queued"), chat.SendOptions{Delivery: chat.Queued}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(ctx, chat.Text("finish-queued"), chat.SendOptions{Delivery: chat.Steer}); err != nil {
		t.Fatal(err)
	}
	starts, ends := 0, 0
	for ends < 2 {
		select {
		case e := <-s.Events():
			if e.Type == "turn_started" {
				starts++
			}
			if e.Type == "turn_ended" {
				ends++
			}
		case <-ctx.Done():
			t.Fatal("missing queued lifecycle")
		}
	}
	if starts != 2 {
		t.Fatalf("%d turn starts for two queued results", starts)
	}
}
