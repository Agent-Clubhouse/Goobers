package speechnotify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

type nativeSystem interface {
	LookPath(string) (string, error)
	Output(context.Context, string, ...string) ([]byte, error)
	Run(context.Context, string, []string, string) error
	AudioSession(context.Context, string) (bool, string)
}

type execSystem struct{}

const (
	sayDelimiterEscapeStart = "[[dlim (( ))]]"
	sayDelimiterEscapeEnd   = "((dlim [[ ]]))"
)

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

func (s execSystem) AudioSession(ctx context.Context, platform string) (bool, string) {
	return newAudioSessionProbe(s.Output, s.Run).available(ctx, platform)
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
	report.AudioAvailable, report.AudioPrerequisite = s.system.AudioSession(ctx, s.platform)

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
	stdin := []string{text}
	switch s.engine {
	case EngineSay:
		args = append(args, "-r", rate)
		if voice != "" {
			args = append(args, "-v", voice)
		}
		stdin[0] = escapeSayCommandDelimiters(text)
	case EngineESpeak:
		args = append(args, "-s", rate)
		if voice != "" {
			args = append(args, "-v", voice)
		}
		stdin = splitESpeakCommandDelimiters(text)
	}
	for _, chunk := range stdin {
		if err := s.system.Run(ctx, executable, args, chunk); err != nil {
			return fmt.Errorf("%s synthesis failed: %w", s.engine, err)
		}
	}
	return nil
}

func escapeSayCommandDelimiters(text string) string {
	var escaped strings.Builder
	escaped.Grow(len(text))
	for i := 0; i < len(text); i++ {
		if i+1 < len(text) && (text[i:i+2] == "[[" || text[i:i+2] == "]]") {
			escaped.WriteString(sayDelimiterEscapeStart)
			escaped.WriteString(text[i : i+2])
			escaped.WriteString(sayDelimiterEscapeEnd)
			i++
			continue
		}
		escaped.WriteByte(text[i])
	}
	return escaped.String()
}

func splitESpeakCommandDelimiters(text string) []string {
	chunks := make([]string, 0, 1)
	start := 0
	for i := 1; i < len(text); i++ {
		if text[i] == text[i-1] && (text[i] == '[' || text[i] == ']') {
			chunks = append(chunks, text[start:i])
			start = i
		}
	}
	return append(chunks, text[start:])
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
	if len(voices) == 0 {
		return "", "", fmt.Errorf("no usable voices reported by %s; install at least one voice and retry", engine)
	}
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
			if len(fields) < 4 {
				continue
			}
			if _, err := strconv.ParseUint(fields[0], 10, 8); err != nil {
				continue
			}
			language := normalizeLanguage(fields[1])
			name := strings.TrimSpace(fields[3])
			if languagePattern.MatchString(language) && name != "" {
				voices = append(voices, voiceInfo{name: name, language: language})
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
