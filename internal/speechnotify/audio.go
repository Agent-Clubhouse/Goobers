package speechnotify

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	macOSAudioPrerequisite = "an active macOS GUI session with a default audio output"
	linuxAudioPrerequisite = "an openable ALSA playback device or reachable PulseAudio/PipeWire Unix session"
)

type audioSessionProbe struct {
	output        func(context.Context, string, ...string) ([]byte, error)
	run           func(context.Context, string, []string, string) error
	getenv        func(string) string
	dialContext   func(context.Context, string, string) (net.Conn, error)
	alsaAvailable func() bool
}

func newAudioSessionProbe(
	output func(context.Context, string, ...string) ([]byte, error),
	run func(context.Context, string, []string, string) error,
) audioSessionProbe {
	dialer := &net.Dialer{Timeout: 250 * time.Millisecond}
	return audioSessionProbe{
		output:        output,
		run:           run,
		getenv:        os.Getenv,
		dialContext:   dialer.DialContext,
		alsaAvailable: alsaPlaybackAvailable,
	}
}

func (p audioSessionProbe) available(ctx context.Context, platform string) (bool, string) {
	switch platform {
	case "darwin":
		return p.macOSAvailable(ctx), macOSAudioPrerequisite
	case "linux":
		return p.linuxAvailable(ctx), linuxAudioPrerequisite
	default:
		return false, "a supported local audio session"
	}
}

func (p audioSessionProbe) macOSAvailable(ctx context.Context) bool {
	uid, err := p.output(ctx, "/usr/bin/id", "-u")
	if err != nil {
		return false
	}
	guiDomain := "gui/" + strings.TrimSpace(string(uid))
	if guiDomain == "gui/" || p.run(ctx, "/bin/launchctl", []string{"print", guiDomain}, "") != nil {
		return false
	}
	audioData, err := p.output(ctx, "/usr/sbin/system_profiler", "SPAudioDataType", "-json")
	return err == nil && hasDefaultMacOSOutput(audioData)
}

func hasDefaultMacOSOutput(data []byte) bool {
	var report any
	if json.Unmarshal(data, &report) != nil {
		return false
	}
	var hasDefaultOutput func(any) bool
	hasDefaultOutput = func(value any) bool {
		switch value := value.(type) {
		case map[string]any:
			for key, child := range value {
				if key == "coreaudio_default_audio_output_device" && child == "spaudio_yes" {
					return true
				}
				if hasDefaultOutput(child) {
					return true
				}
			}
		case []any:
			for _, child := range value {
				if hasDefaultOutput(child) {
					return true
				}
			}
		}
		return false
	}
	return hasDefaultOutput(report)
}

func (p audioSessionProbe) linuxAvailable(ctx context.Context) bool {
	for _, path := range p.linuxSessionSockets() {
		connection, err := p.dialContext(ctx, "unix", path)
		if err == nil {
			_ = connection.Close()
			return true
		}
	}
	return p.alsaAvailable()
}

func (p audioSessionProbe) linuxSessionSockets() []string {
	var paths []string
	add := func(path string) {
		path = strings.TrimSpace(strings.TrimPrefix(path, "unix:"))
		if filepath.IsAbs(path) {
			paths = append(paths, path)
		}
	}

	add(p.getenv("PULSE_SERVER"))
	if pulseRuntime := p.getenv("PULSE_RUNTIME_PATH"); pulseRuntime != "" {
		add(filepath.Join(pulseRuntime, "native"))
	}
	runtimeDir := p.getenv("XDG_RUNTIME_DIR")
	pipewireRemote := p.getenv("PIPEWIRE_REMOTE")
	if filepath.IsAbs(pipewireRemote) {
		add(pipewireRemote)
	}
	if runtimeDir != "" {
		add(filepath.Join(runtimeDir, "pulse", "native"))
		if pipewireRemote == "" {
			pipewireRemote = "pipewire-0"
		}
		if !filepath.IsAbs(pipewireRemote) {
			add(filepath.Join(runtimeDir, pipewireRemote))
		}
	}
	return paths
}
