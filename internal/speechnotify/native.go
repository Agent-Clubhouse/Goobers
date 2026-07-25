package speechnotify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

type nativeSystem interface {
	LookPath(string) (string, error)
	Output(context.Context, string, ...string) ([]byte, error)
	Run(context.Context, string, []string, string) error
	AudioSession(string) (bool, string)
}

type execSystem struct{}

func (execSystem) LookPath(name string) (string, error) {
	return exec.LookPath(name)
}

func (execSystem) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%s: %w", sanitizeError(errors.New(detail)), err)
		}
		return nil, err
	}
	return output, nil
}

func (execSystem) Run(ctx context.Context, name string, args []string, stdin string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("%s: %w", sanitizeError(errors.New(detail)), err)
		}
		return err
	}
	return nil
}

func (execSystem) AudioSession(platform string) (bool, string) {
	switch platform {
	case "darwin":
		return true, "an active macOS user audio output; confirm it with `goobers speech test`"
	case "linux":
		const prerequisite = "an ALSA device or active PulseAudio/PipeWire session"
		if _, err := os.Stat("/dev/snd"); err == nil {
			return true, prerequisite
		}
		if os.Getenv("PULSE_SERVER") != "" || os.Getenv("PIPEWIRE_REMOTE") != "" {
			return true, prerequisite
		}
		if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
			for _, socket := range []string{"pulse/native", "pipewire-0"} {
				if _, err := os.Stat(filepath.Join(runtimeDir, socket)); err == nil {
					return true, prerequisite
				}
			}
		}
		return false, prerequisite
	default:
		return false, "a supported local audio session"
	}
}

type nativeSynthesizer struct {
	platform string
	engine   string
	system   nativeSystem

	mu         sync.RWMutex
	executable string
	voice      string
}

// NewNative returns the bounded native adapter for the current platform.
func NewNative(config Config, recorder Recorder) (*Sink, error) {
	return newNativeForPlatform(runtime.GOOS, config, recorder, execSystem{})
}

func newNativeForPlatform(platform string, config Config, recorder Recorder, system nativeSystem) (*Sink, error) {
	engine := config.EffectiveEngine()
	if engine == EngineAuto {
		switch platform {
		case "darwin":
			engine = EngineSay
		case "linux":
			engine = EngineESpeak
		default:
			return nil, fmt.Errorf("local speech is not supported on %s (supported: macOS say, Linux eSpeak)", platform)
		}
	}
	if engine == EngineSay && platform != "darwin" {
		return nil, fmt.Errorf("speech engine say requires macOS, current platform is %s", platform)
	}
	if engine == EngineESpeak && platform != "linux" {
		return nil, fmt.Errorf("speech engine espeak requires Linux, current platform is %s", platform)
	}
	return New(config, &nativeSynthesizer{
		platform: platform,
		engine:   engine,
		system:   system,
	}, recorder)
}

func (s *nativeSynthesizer) Name() string { return s.engine }

func (s *nativeSynthesizer) Preflight(ctx context.Context, config Config) (Preflight, error) {
	report := Preflight{
		Engine:   s.engine,
		Rate:     config.EffectiveRate(),
		Voice:    "system default",
		Language: "system default",
	}
	report.AudioAvailable, report.AudioPrerequisite = s.system.AudioSession(s.platform)

	var (
		executable string
		output     []byte
		err        error
	)
	switch s.engine {
	case EngineSay:
		executable, err = s.system.LookPath("say")
		if err == nil {
			output, err = s.system.Output(ctx, executable, "-v", "?")
		}
	case EngineESpeak:
		for _, candidate := range []string{"espeak-ng", "espeak"} {
			executable, err = s.system.LookPath(candidate)
			if err == nil {
				break
			}
		}
		if err == nil {
			output, err = s.system.Output(ctx, executable, "--voices")
		}
	}
	if executable != "" {
		report.Executable = executable
	}
	if err != nil {
		return report, fmt.Errorf("speech preflight: %s executable or voice list unavailable: %w; install the engine and retry", s.engine, err)
	}

	voice, language, err := selectVoice(s.engine, output, config.Voice, config.Language)
	if err != nil {
		return report, fmt.Errorf("speech preflight: %w", err)
	}
	if voice != "" {
		report.Voice = voice
	}
	if language != "" {
		report.Language = language
	}
	if !report.AudioAvailable {
		return report, fmt.Errorf("speech preflight: no audio session detected; requires %s", report.AudioPrerequisite)
	}

	s.mu.Lock()
	s.executable = executable
	s.voice = voice
	s.mu.Unlock()
	return report, nil
}

func (s *nativeSynthesizer) Synthesize(ctx context.Context, config Config, text string) error {
	s.mu.RLock()
	executable := s.executable
	voice := s.voice
	s.mu.RUnlock()
	if executable == "" {
		return errorsPreflightRequired(s.engine)
	}

	rate := strconv.Itoa(config.EffectiveRate())
	var args []string
	switch s.engine {
	case EngineSay:
		args = append(args, "-r", rate)
		if voice != "" {
			args = append(args, "-v", voice)
		}
	case EngineESpeak:
		args = append(args, "-s", rate)
		if voice != "" {
			args = append(args, "-v", voice)
		}
	}
	if err := s.system.Run(ctx, executable, args, text); err != nil {
		return fmt.Errorf("%s synthesis failed: %w", s.engine, err)
	}
	return nil
}

func errorsPreflightRequired(engine string) error {
	return fmt.Errorf("%s synthesis requires a successful preflight", engine)
}

type voiceInfo struct {
	name     string
	language string
}

func selectVoice(engine string, output []byte, requestedVoice, requestedLanguage string) (string, string, error) {
	voices := parseVoices(engine, string(output))
	normalizedLanguage := normalizeLanguage(requestedLanguage)
	if requestedVoice != "" {
		for _, voice := range voices {
			if voice.name != requestedVoice {
				continue
			}
			if normalizedLanguage != "" && !languageMatches(voice.language, normalizedLanguage) {
				return "", "", fmt.Errorf("voice %q is language %s, not requested language %s", requestedVoice, voice.language, requestedLanguage)
			}
			return voice.name, displayLanguage(voice.language), nil
		}
		return "", "", fmt.Errorf("voice %q is not available for %s", requestedVoice, engine)
	}
	if normalizedLanguage != "" {
		for _, voice := range voices {
			if languageMatches(voice.language, normalizedLanguage) {
				return voice.name, displayLanguage(voice.language), nil
			}
		}
		return "", "", fmt.Errorf("language %q is not available for %s", requestedLanguage, engine)
	}
	return "", "", nil
}

func parseVoices(engine, output string) []voiceInfo {
	var voices []voiceInfo
	for _, line := range strings.Split(output, "\n") {
		definition, _, _ := strings.Cut(line, "#")
		fields := strings.Fields(definition)
		switch engine {
		case EngineSay:
			if len(fields) >= 2 {
				language := normalizeLanguage(fields[len(fields)-1])
				if languagePattern.MatchString(language) {
					voices = append(voices, voiceInfo{
						name:     strings.Join(fields[:len(fields)-1], " "),
						language: language,
					})
				}
			}
		case EngineESpeak:
			if len(fields) >= 4 && strings.ToLower(fields[0]) != "pty" {
				voices = append(voices, voiceInfo{name: fields[3], language: normalizeLanguage(fields[1])})
			}
		}
	}
	return voices
}

func normalizeLanguage(language string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(language), "_", "-"))
}

func displayLanguage(language string) string {
	if language == "" {
		return ""
	}
	return language
}

func languageMatches(available, requested string) bool {
	return available == requested ||
		strings.HasPrefix(available, requested+"-") ||
		strings.HasPrefix(requested, available+"-")
}
