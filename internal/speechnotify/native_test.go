package speechnotify

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeNativeSystem struct {
	paths          map[string]string
	output         string
	outputErr      error
	runErr         error
	audioAvailable bool
	prerequisite   string

	runName  string
	runArgs  []string
	runStdin string
}

func (s *fakeNativeSystem) LookPath(name string) (string, error) {
	if path := s.paths[name]; path != "" {
		return path, nil
	}
	return "", errors.New("not found")
}

func (s *fakeNativeSystem) Output(context.Context, string, ...string) ([]byte, error) {
	return []byte(s.output), s.outputErr
}

func (s *fakeNativeSystem) Run(_ context.Context, name string, args []string, stdin string) error {
	s.runName = name
	s.runArgs = append([]string(nil), args...)
	s.runStdin = stdin
	return s.runErr
}

func (s *fakeNativeSystem) AudioSession(context.Context, string) (bool, string) {
	return s.audioAvailable, s.prerequisite
}

func TestAudioSessionProbeRejectsHeadlessMacOS(t *testing.T) {
	profilerCalled := false
	probe := audioSessionProbe{
		output: func(_ context.Context, name string, _ ...string) ([]byte, error) {
			if name == "/usr/sbin/system_profiler" {
				profilerCalled = true
			}
			return []byte("501\n"), nil
		},
		run: func(context.Context, string, []string, string) error {
			return errors.New("GUI domain unavailable")
		},
	}
	available, prerequisite := probe.available(context.Background(), "darwin")
	if available || prerequisite != macOSAudioPrerequisite || profilerCalled {
		t.Fatalf("available = %t, prerequisite = %q, profiler called = %t", available, prerequisite, profilerCalled)
	}
}

func TestAudioSessionProbeRequiresMacOSDefaultOutput(t *testing.T) {
	for _, test := range []struct {
		name      string
		report    string
		available bool
	}{
		{
			name:      "default output",
			report:    `{"SPAudioDataType":[{"_name":"Speakers","coreaudio_default_audio_output_device":"spaudio_yes"}]}`,
			available: true,
		},
		{
			name:   "input only",
			report: `{"SPAudioDataType":[{"_name":"Microphone","coreaudio_default_audio_input_device":"spaudio_yes"}]}`,
		},
		{name: "invalid report", report: "not-json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe := audioSessionProbe{
				output: func(_ context.Context, name string, _ ...string) ([]byte, error) {
					if name == "/usr/bin/id" {
						return []byte("501\n"), nil
					}
					return []byte(test.report), nil
				},
				run: func(context.Context, string, []string, string) error { return nil },
			}
			available, _ := probe.available(context.Background(), "darwin")
			if available != test.available {
				t.Fatalf("available = %t, want %t", available, test.available)
			}
		})
	}
}

func TestAudioSessionProbeRejectsInvalidLinuxEndpoints(t *testing.T) {
	staleSocket := filepath.Join(t.TempDir(), "pulse.sock")
	if err := os.WriteFile(staleSocket, []byte("not a socket"), 0o600); err != nil {
		t.Fatalf("write stale endpoint: %v", err)
	}
	environment := map[string]string{
		"PULSE_SERVER":    "unix:" + staleSocket,
		"PIPEWIRE_REMOTE": filepath.Join(t.TempDir(), "missing-pipewire.sock"),
	}
	dialer := &net.Dialer{Timeout: 10 * time.Millisecond}
	probe := audioSessionProbe{
		getenv:        func(key string) string { return environment[key] },
		dialContext:   dialer.DialContext,
		alsaAvailable: func() bool { return false },
	}
	available, prerequisite := probe.available(context.Background(), "linux")
	if available || prerequisite != linuxAudioPrerequisite {
		t.Fatalf("available = %t, prerequisite = %q", available, prerequisite)
	}
}

func TestAudioSessionProbeUsesAbsolutePipeWireEndpoint(t *testing.T) {
	endpoint := filepath.Join(t.TempDir(), "pipewire.sock")
	var dialed string
	probe := audioSessionProbe{
		getenv: func(key string) string {
			if key == "PIPEWIRE_REMOTE" {
				return endpoint
			}
			return ""
		},
		dialContext: func(_ context.Context, _, address string) (net.Conn, error) {
			dialed = address
			client, server := net.Pipe()
			_ = server.Close()
			return client, nil
		},
		alsaAvailable: func() bool { return false },
	}
	available, _ := probe.available(context.Background(), "linux")
	if !available || dialed != endpoint {
		t.Fatalf("available = %t, dialed = %q, want %q", available, dialed, endpoint)
	}
}

func TestSayPreflightAndExactStdinDelivery(t *testing.T) {
	system := &fakeNativeSystem{
		paths:          map[string]string{"say": "/usr/bin/say"},
		output:         "Alex en_US # Most people recognize me\nSamantha en_US # Hello\nBad News en_US # Boo\nAmelie fr_CA # Bonjour\n",
		audioAvailable: true,
		prerequisite:   "macOS audio",
	}
	config := Config{Engine: EngineSay, Voice: "Samantha", Language: "en-US", Rate: 205}
	sink, err := newNativeForPlatform("darwin", config, nil, system)
	if err != nil {
		t.Fatalf("newNativeForPlatform: %v", err)
	}
	report, err := sink.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if report.Executable != "/usr/bin/say" || report.Voice != "Samantha" ||
		report.Language != "en-us" || report.Rate != 205 || !report.AudioAvailable {
		t.Fatalf("report = %+v", report)
	}
	text := `literal; $(touch /tmp/nope) "quoted" — 2 µs`
	if _, err := sink.Deliver(context.Background(), Request{NotificationID: "say-1", Text: text}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if system.runName != "/usr/bin/say" ||
		strings.Join(system.runArgs, "|") != "-r|205|-v|Samantha" ||
		system.runStdin != text {
		t.Fatalf("run = %q %#v stdin %q", system.runName, system.runArgs, system.runStdin)
	}
	for _, arg := range system.runArgs {
		if strings.Contains(arg, "touch") {
			t.Fatalf("speech text leaked into command arguments: %#v", system.runArgs)
		}
	}

}

func TestSayPreflightSupportsMultiwordVoiceNames(t *testing.T) {
	system := &fakeNativeSystem{
		paths:          map[string]string{"say": "/usr/bin/say"},
		output:         "Bad News en_US # Boo\n",
		audioAvailable: true,
	}
	sink, err := newNativeForPlatform("darwin", Config{Voice: "Bad News"}, nil, system)
	if err != nil {
		t.Fatalf("newNativeForPlatform: %v", err)
	}
	report, err := sink.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if report.Voice != "Bad News" || report.Language != "en-us" {
		t.Fatalf("report = %+v", report)
	}
}

func TestESpeakAutoSelectionUsesAvailableBinaryAndLanguage(t *testing.T) {
	system := &fakeNativeSystem{
		paths:          map[string]string{"espeak": "/usr/bin/espeak"},
		output:         "Pty Language Age/Gender VoiceName File Other Languages\n 5  en-us M english-us en-us\n 5  fr-fr M french fr\n",
		audioAvailable: true,
		prerequisite:   "Linux audio",
	}
	config := Config{Language: "en-US"}
	sink, err := newNativeForPlatform("linux", config, nil, system)
	if err != nil {
		t.Fatalf("newNativeForPlatform: %v", err)
	}
	report, err := sink.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if report.Engine != EngineESpeak || report.Executable != "/usr/bin/espeak" || report.Voice != "english-us" {
		t.Fatalf("report = %+v", report)
	}
	if _, err := sink.Deliver(context.Background(), Request{NotificationID: "linux-1", Text: "-w /tmp/nope"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if strings.Join(system.runArgs, "|") != "-s|180|-v|english-us" || system.runStdin != "-w /tmp/nope" {
		t.Fatalf("args = %#v, stdin = %q", system.runArgs, system.runStdin)
	}
}

func TestPreflightFailuresAreActionable(t *testing.T) {
	t.Run("missing engine", func(t *testing.T) {
		sink, err := newNativeForPlatform("darwin", Config{}, nil, &fakeNativeSystem{
			paths:          map[string]string{},
			audioAvailable: true,
		})
		if err != nil {
			t.Fatalf("newNativeForPlatform: %v", err)
		}
		report, err := sink.Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "install the engine and retry") || report.Engine != EngineSay {
			t.Fatalf("Preflight = (%+v, %v)", report, err)
		}
	})
	t.Run("headless", func(t *testing.T) {
		sink, err := newNativeForPlatform("linux", Config{}, nil, &fakeNativeSystem{
			paths:        map[string]string{"espeak-ng": "/usr/bin/espeak-ng"},
			output:       "Pty Language Age/Gender VoiceName File Other Languages\n 5 en M english en\n",
			prerequisite: "an active PipeWire session",
		})
		if err != nil {
			t.Fatalf("newNativeForPlatform: %v", err)
		}
		report, err := sink.Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "no audio session detected") ||
			report.AudioAvailable || report.AudioPrerequisite == "" {
			t.Fatalf("Preflight = (%+v, %v)", report, err)
		}
	})
}

func TestPreflightRejectsUnavailableVoiceAndLanguageMismatch(t *testing.T) {
	system := &fakeNativeSystem{
		paths:          map[string]string{"say": "/usr/bin/say"},
		output:         "Amelie fr_CA # Bonjour\n",
		audioAvailable: true,
	}
	for _, test := range []struct {
		name   string
		config Config
		want   string
	}{
		{name: "missing", config: Config{Voice: "Samantha"}, want: "is not available"},
		{name: "mismatch", config: Config{Voice: "Amelie", Language: "en-US"}, want: "not requested language"},
	} {
		t.Run(test.name, func(t *testing.T) {
			sink, err := newNativeForPlatform("darwin", test.config, nil, system)
			if err != nil {
				t.Fatalf("newNativeForPlatform: %v", err)
			}
			if _, err := sink.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Preflight error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestNativePlatformAndEngineAreBounded(t *testing.T) {
	for _, test := range []struct {
		platform string
		config   Config
		want     string
	}{
		{platform: "windows", config: Config{}, want: "not supported"},
		{platform: "linux", config: Config{Engine: EngineSay}, want: "requires macOS"},
		{platform: "darwin", config: Config{Engine: EngineESpeak}, want: "requires Linux"},
	} {
		if _, err := newNativeForPlatform(test.platform, test.config, nil, &fakeNativeSystem{}); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("newNativeForPlatform(%s, %+v) error = %v, want %q", test.platform, test.config, err, test.want)
		}
	}
}
