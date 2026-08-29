package desktopnotify

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type recordingRunner struct {
	lookup  string
	path    string
	pathErr error
	name    string
	args    []string
	err     error
}

func (r *recordingRunner) LookPath(name string) (string, error) {
	r.lookup = name
	return r.path, r.pathErr
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) error {
	r.name = name
	r.args = append([]string(nil), args...)
	return r.err
}

func TestNewForPlatformUnsupported(t *testing.T) {
	if notifier, supported := newForPlatform("windows", &recordingRunner{}); supported || notifier != nil {
		t.Fatalf("newForPlatform(windows) = (%v, %v), want (nil, false)", notifier, supported)
	}
}

func TestNewForPlatformLinuxWithoutNotifySend(t *testing.T) {
	run := &recordingRunner{pathErr: errors.New("not found")}
	if notifier, supported := newForPlatform("linux", run); supported || notifier != nil {
		t.Fatalf("newForPlatform(linux) = (%v, %v), want (nil, false)", notifier, supported)
	}
}

func TestMacOSNotifierUsesOSAScriptAndEscapesContent(t *testing.T) {
	run := &recordingRunner{}
	notifier, supported := newForPlatform("darwin", run)
	if !supported {
		t.Fatal("darwin notifier reported unsupported")
	}
	err := notifier.Notify(context.Background(), Message{
		Title: `Goobers "failed"`,
		Body:  "workflow [12345678]\nquote \" and slash \\",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if run.name != "osascript" {
		t.Fatalf("command = %q, want osascript", run.name)
	}
	if len(run.args) != 2 || run.args[0] != "-e" {
		t.Fatalf("args = %#v, want [-e script]", run.args)
	}
	want := `display notification "workflow [12345678]\nquote \" and slash \\" with title "Goobers \"failed\""`
	if run.args[1] != want {
		t.Fatalf("script = %q, want %q", run.args[1], want)
	}
}

func TestMacOSNotifierReturnsCommandFailure(t *testing.T) {
	run := &recordingRunner{err: errors.New("osascript unavailable")}
	notifier, _ := newForPlatform("darwin", run)
	err := notifier.Notify(context.Background(), Message{Title: "Goobers", Body: "failed"})
	if err == nil || !strings.Contains(err.Error(), "osascript unavailable") {
		t.Fatalf("Notify error = %v", err)
	}
}

func TestLinuxNotifierUsesNotifySend(t *testing.T) {
	run := &recordingRunner{path: "/usr/bin/notify-send"}
	notifier, supported := newForPlatform("linux", run)
	if !supported {
		t.Fatal("linux notifier reported unsupported")
	}
	if run.lookup != "notify-send" {
		t.Fatalf("executable lookup = %q, want notify-send", run.lookup)
	}
	err := notifier.Notify(context.Background(), Message{
		Title: `Goobers "failed"`,
		Body:  "workflow [12345678]\nquote \" and slash \\",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if run.name != "/usr/bin/notify-send" {
		t.Fatalf("command = %q, want /usr/bin/notify-send", run.name)
	}
	want := []string{`Goobers "failed"`, "workflow [12345678]\nquote \" and slash \\"}
	if len(run.args) != len(want) || run.args[0] != want[0] || run.args[1] != want[1] {
		t.Fatalf("args = %#v, want %#v", run.args, want)
	}
}

func TestLinuxNotifierReturnsCommandFailure(t *testing.T) {
	run := &recordingRunner{
		path: "/usr/bin/notify-send",
		err:  errors.New("notification service unavailable"),
	}
	notifier, _ := newForPlatform("linux", run)
	err := notifier.Notify(context.Background(), Message{Title: "Goobers", Body: "failed"})
	if err == nil || !strings.Contains(err.Error(), "notification service unavailable") {
		t.Fatalf("Notify error = %v", err)
	}
}
