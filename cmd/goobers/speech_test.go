package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/speechnotify"
	speechtest "github.com/goobers/goobers/test/testsupport/speechnotify"
)

func speechTestInstance(t *testing.T, config speechnotify.Config) string {
	t.Helper()
	root := t.TempDir()
	if _, err := instance.Init(root); err != nil {
		t.Fatalf("Init: %v", err)
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Speech = &config
	if err := instance.WriteConfig(instance.NewLayout(root).ConfigFile(), cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	return root
}

func installFakeSpeechSink(t *testing.T, fake *speechtest.FakeSynthesizer) {
	t.Helper()
	original := newNativeSpeechSink
	newNativeSpeechSink = func(config speechnotify.Config, recorder speechnotify.Recorder) (*speechnotify.Sink, error) {
		return speechnotify.New(config, fake, recorder)
	}
	t.Cleanup(func() { newNativeSpeechSink = original })
}

func TestSpeechPreflightWorksWhileDisabled(t *testing.T) {
	root := speechTestInstance(t, speechnotify.Config{Language: "en-US", Rate: 205})
	fake := &speechtest.FakeSynthesizer{Report: speechnotify.Preflight{
		Engine:            "fake",
		Executable:        "in-process",
		Voice:             "CI",
		Language:          "en-US",
		Rate:              205,
		AudioPrerequisite: "none",
		AudioAvailable:    true,
	}}
	installFakeSpeechSink(t, fake)

	code, stdout, stderr := runArgs(t, "speech", "preflight", root)
	if code != 0 || stderr != "" {
		t.Fatalf("speech preflight = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	for _, want := range []string{
		"engine: fake",
		"executable: in-process",
		"voice: CI",
		"language: en-US",
		"rate: 205 words/minute",
		"audio available: true",
	} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("preflight output %q missing %q", stdout, want)
		}
	}
	if len(fake.Utterances()) != 0 {
		t.Fatalf("preflight emitted speech: %#v", fake.Utterances())
	}
}

func TestSpeechTestUsesFixedPhraseAndWritesReceipts(t *testing.T) {
	root := speechTestInstance(t, speechnotify.Config{})
	fake := &speechtest.FakeSynthesizer{}
	installFakeSpeechSink(t, fake)

	code, stdout, stderr := runArgs(t, "speech", "test", "--json", root)
	if code != 0 || stderr != "" {
		t.Fatalf("speech test = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if got := fake.Utterances(); len(got) != 1 || got[0] != speechTestPhrase {
		t.Fatalf("utterances = %#v, want fixed phrase", got)
	}
	var receipt speechnotify.Receipt
	if err := json.Unmarshal([]byte(stdout), &receipt); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	if receipt.Status != speechnotify.StatusDelivered || receipt.NotificationID != "speech-test" {
		t.Fatalf("receipt = %+v", receipt)
	}
	raw, err := os.ReadFile(filepath.Join(root, instance.SchedulerDirName, speechnotify.ReceiptFileName))
	if err != nil {
		t.Fatalf("read receipt log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"status":"started"`) ||
		!strings.Contains(lines[1], `"status":"delivered"`) ||
		strings.Contains(string(raw), speechTestPhrase) {
		t.Fatalf("receipt log = %q", raw)
	}
}

func TestSpeechPreflightFailureIsActionable(t *testing.T) {
	root := speechTestInstance(t, speechnotify.Config{})
	fake := &speechtest.FakeSynthesizer{
		Report: speechnotify.Preflight{
			Engine:            "fake",
			AudioPrerequisite: "an active local audio session",
		},
		PreflightErr: errors.New("no audio session detected"),
	}
	installFakeSpeechSink(t, fake)

	code, stdout, stderr := runArgs(t, "speech", "preflight", root)
	if code != 1 || !strings.Contains(stdout, "audio prerequisite: an active local audio session") ||
		!strings.Contains(stderr, "no audio session detected") {
		t.Fatalf("speech preflight = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestSpeechTestRejectsArbitraryPhrase(t *testing.T) {
	root := speechTestInstance(t, speechnotify.Config{})
	code, _, stderr := runArgs(t, "speech", "test", "say this instead", root)
	if code != 2 || !strings.Contains(stderr, "Usage: goobers speech test") {
		t.Fatalf("speech test = %d, stderr %q", code, stderr)
	}
}
