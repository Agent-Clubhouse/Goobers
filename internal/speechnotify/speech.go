// Package speechnotify delivers exact notification text through bounded local
// speech synthesis adapters.
package speechnotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

// Supported speech engines and bounded configuration defaults.
const (
	EngineAuto     = "auto"
	EngineSay      = "say"
	EngineESpeak   = "espeak"
	DefaultRate    = 180
	MinimumRate    = 80
	MaximumRate    = 450
	MaxTextBytes   = 4096
	DefaultTimeout = 15 * time.Second
	// ReceiptFileName is the instance scheduler-side speech receipt log.
	ReceiptFileName    = "speech-receipts.jsonl"
	maximumTimeout     = 2 * time.Minute
	queueCapacity      = 32
	receiptVersion     = "v1"
	receiptFileMaxSize = 1 << 20
	receiptLockSuffix  = ".lock"
)

var languagePattern = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)

// Config selects and configures the local speech engine. Speech remains
// disabled unless Enabled is explicitly set.
type Config struct {
	Enabled  bool   `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Engine   string `json:"engine,omitempty" yaml:"engine,omitempty"`
	Voice    string `json:"voice,omitempty" yaml:"voice,omitempty"`
	Language string `json:"language,omitempty" yaml:"language,omitempty"`
	Rate     int    `json:"rate,omitempty" yaml:"rate,omitempty"`
	Timeout  string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// Validate rejects values that could become unbounded or ambiguous adapter
// options.
func (c Config) Validate() error {
	switch c.EffectiveEngine() {
	case EngineAuto, EngineSay, EngineESpeak:
	default:
		return fmt.Errorf("engine %q is not supported (want auto, say, or espeak)", c.Engine)
	}
	if c.Voice != "" {
		if strings.HasPrefix(c.Voice, "-") {
			return errors.New("voice must not begin with '-'")
		}
		if utf8.RuneCountInString(c.Voice) > 64 {
			return errors.New("voice must be 64 characters or fewer")
		}
		for _, r := range c.Voice {
			if unicode.IsControl(r) {
				return errors.New("voice must not contain control characters")
			}
		}
	}
	if c.Language != "" && !languagePattern.MatchString(c.Language) {
		return fmt.Errorf("language %q must be a BCP 47-style tag such as en-US", c.Language)
	}
	rate := c.EffectiveRate()
	if rate < MinimumRate || rate > MaximumRate {
		return fmt.Errorf("rate must be between %d and %d words per minute", MinimumRate, MaximumRate)
	}
	if _, err := c.TimeoutDuration(); err != nil {
		return err
	}
	return nil
}

// EffectiveEngine returns the configured engine, defaulting to auto selection.
func (c Config) EffectiveEngine() string {
	engine := strings.ToLower(strings.TrimSpace(c.Engine))
	if engine == "" {
		return EngineAuto
	}
	return engine
}

// EffectiveRate returns the configured rate or the cross-engine default.
func (c Config) EffectiveRate() int {
	if c.Rate == 0 {
		return DefaultRate
	}
	return c.Rate
}

// TimeoutDuration returns the configured delivery timeout.
func (c Config) TimeoutDuration() (time.Duration, error) {
	if c.Timeout == "" {
		return DefaultTimeout, nil
	}
	timeout, err := time.ParseDuration(c.Timeout)
	if err != nil {
		return 0, fmt.Errorf("timeout %q: %w", c.Timeout, err)
	}
	if timeout < time.Second || timeout > maximumTimeout {
		return 0, fmt.Errorf("timeout must be between 1s and %s", maximumTimeout)
	}
	return timeout, nil
}

// Request is the exact validated speech payload. Text is never rewritten by
// the sink.
type Request struct {
	NotificationID string
	Text           string
}

func (r Request) validate() error {
	if strings.TrimSpace(r.NotificationID) == "" {
		return errors.New("notification id is required")
	}
	if r.Text == "" {
		return errors.New("speech text is required")
	}
	if len(r.Text) > MaxTextBytes {
		return fmt.Errorf("speech text exceeds %d bytes", MaxTextBytes)
	}
	return nil
}

// Preflight reports the selected adapter and every local prerequisite checked
// without emitting sound.
type Preflight struct {
	Engine            string `json:"engine"`
	Executable        string `json:"executable"`
	Voice             string `json:"voice"`
	Language          string `json:"language"`
	Rate              int    `json:"rate"`
	AudioPrerequisite string `json:"audioPrerequisite"`
	AudioAvailable    bool   `json:"audioAvailable"`
}

// Status is a delivery receipt state.
type Status string

// Speech delivery receipt statuses.
const (
	StatusStarted   Status = "started"
	StatusDelivered Status = "delivered"
	StatusFailed    Status = "failed"
)

// Receipt records one speech delivery transition without storing the spoken
// text.
type Receipt struct {
	Version        string     `json:"version"`
	NotificationID string     `json:"notificationId"`
	Engine         string     `json:"engine"`
	Status         Status     `json:"status"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	CompletedAt    *time.Time `json:"completedAt,omitempty"`
	DurationMillis int64      `json:"durationMillis"`
	Error          string     `json:"error,omitempty"`
}

// Recorder persists or observes receipt transitions.
type Recorder interface {
	Record(context.Context, Receipt) error
}

type nopRecorder struct{}

func (nopRecorder) Record(context.Context, Receipt) error { return nil }

// FileRecorder appends receipts as one JSON object per line. Recorders targeting
// the same file serialize and sync writes before reporting success. The log is
// truncated before an append that would grow it beyond the fixed retention bound.
type FileRecorder struct {
	path string
}

// NewFileRecorder creates a lazy JSONL recorder at path.
func NewFileRecorder(path string) *FileRecorder {
	return &FileRecorder{path: path}
}

// Record appends and syncs one receipt.
func (r *FileRecorder) Record(ctx context.Context, receipt Receipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	line, err := json.Marshal(receipt)
	if err != nil {
		return fmt.Errorf("encode speech receipt: %w", err)
	}
	line = append(line, '\n')

	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return fmt.Errorf("create speech receipt directory: %w", err)
	}
	held, err := acquireReceiptLock(ctx, r.path+receiptLockSuffix)
	if err != nil {
		return err
	}
	defer func() { _ = held.Release() }()

	file, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open speech receipt log: %w", err)
	}
	if len(line) > receiptFileMaxSize {
		_ = file.Close()
		return fmt.Errorf("speech receipt exceeds %d-byte log limit", receiptFileMaxSize)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat speech receipt log: %w", err)
	}
	if info.Size() > int64(receiptFileMaxSize-len(line)) {
		if err := file.Truncate(0); err != nil {
			_ = file.Close()
			return fmt.Errorf("truncate speech receipt log: %w", err)
		}
	}
	if _, err := file.Write(line); err != nil {
		_ = file.Close()
		return fmt.Errorf("append speech receipt: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("sync speech receipt: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close speech receipt log: %w", err)
	}
	return nil
}

func acquireReceiptLock(ctx context.Context, path string) (*platformlock.Handle, error) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		held, err := platformlock.TryAcquire(path)
		if err == nil {
			return held, nil
		}
		if !errors.Is(err, platformlock.ErrHeld) {
			return nil, fmt.Errorf("acquire speech receipt log lock: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// Synthesizer is the bounded adapter contract implemented by native engines
// and FakeSynthesizer.
type Synthesizer interface {
	Name() string
	Preflight(context.Context, Config) (Preflight, error)
	Synthesize(context.Context, Config, string) error
}

type deliveryJob struct {
	ctx     context.Context
	request Request
	result  chan deliveryResult
	state   atomic.Uint32
}

const (
	jobQueued uint32 = iota
	jobStarted
	jobCancelled
)

type deliveryResult struct {
	receipt Receipt
	err     error
}

// Sink validates requests, admits a bounded queue, and delivers accepted jobs
// one at a time in FIFO admission order. Active speech is never interrupted by
// a newer alert.
type Sink struct {
	config      Config
	synthesizer Synthesizer
	recorder    Recorder
	jobs        chan *deliveryJob
	start       sync.Once
}

// New returns a speech sink backed by synthesizer.
func New(config Config, synthesizer Synthesizer, recorder Recorder) (*Sink, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if synthesizer == nil {
		return nil, errors.New("speech synthesizer is required")
	}
	if recorder == nil {
		recorder = nopRecorder{}
	}
	sink := &Sink{
		config:      config,
		synthesizer: synthesizer,
		recorder:    recorder,
		jobs:        make(chan *deliveryJob, queueCapacity),
	}
	return sink, nil
}

// Preflight checks the selected engine without speaking.
func (s *Sink) Preflight(ctx context.Context) (Preflight, error) {
	return s.synthesizer.Preflight(ctx, s.config)
}

// Deliver queues exact request text and waits for its final receipt.
func (s *Sink) Deliver(ctx context.Context, request Request) (Receipt, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := request.validate(); err != nil {
		return s.reject(ctx, request.NotificationID, err)
	}
	if err := ctx.Err(); err != nil {
		return s.reject(ctx, request.NotificationID, err)
	}
	s.start.Do(func() { go s.run() })
	job := &deliveryJob{ctx: ctx, request: request, result: make(chan deliveryResult, 1)}
	select {
	case s.jobs <- job:
	default:
		return s.reject(ctx, request.NotificationID, errors.New("speech delivery queue is full"))
	}
	select {
	case result := <-job.result:
		return result.receipt, result.err
	case <-ctx.Done():
		if job.state.CompareAndSwap(jobQueued, jobCancelled) {
			return s.reject(ctx, request.NotificationID, ctx.Err())
		}
		result := <-job.result
		return result.receipt, result.err
	}
}

func (s *Sink) reject(ctx context.Context, notificationID string, cause error) (Receipt, error) {
	completed := time.Now().UTC()
	receipt := Receipt{
		Version:        receiptVersion,
		NotificationID: notificationID,
		Engine:         s.synthesizer.Name(),
		Status:         StatusFailed,
		CompletedAt:    &completed,
		Error:          sanitizeError(cause),
	}
	if err := s.recorder.Record(context.WithoutCancel(ctx), receipt); err != nil {
		return receipt, fmt.Errorf("%s; record speech receipt: %w", receipt.Error, err)
	}
	return receipt, errors.New(receipt.Error)
}

func (s *Sink) run() {
	for job := range s.jobs {
		if job.state.CompareAndSwap(jobQueued, jobStarted) {
			job.result <- s.deliver(job)
		}
	}
}

func (s *Sink) deliver(job *deliveryJob) deliveryResult {
	started := time.Now().UTC()
	start := Receipt{
		Version:        receiptVersion,
		NotificationID: job.request.NotificationID,
		Engine:         s.synthesizer.Name(),
		Status:         StatusStarted,
		StartedAt:      &started,
	}
	if err := s.recorder.Record(context.WithoutCancel(job.ctx), start); err != nil {
		failed := finishReceipt(start, time.Now().UTC(), fmt.Errorf("record start receipt: %w", err))
		return deliveryResult{receipt: failed, err: errors.New(failed.Error)}
	}

	timeout, _ := s.config.TimeoutDuration()
	ctx, cancel := context.WithTimeout(job.ctx, timeout)
	err := s.synthesizer.Synthesize(ctx, s.config, job.request.Text)
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = ctxErr
	}
	cancel()

	final := finishReceipt(start, time.Now().UTC(), err)
	if recordErr := s.recorder.Record(context.WithoutCancel(job.ctx), final); recordErr != nil {
		return deliveryResult{
			receipt: final,
			err:     fmt.Errorf("record final speech receipt: %w", recordErr),
		}
	}
	if final.Status == StatusFailed {
		return deliveryResult{receipt: final, err: errors.New(final.Error)}
	}
	return deliveryResult{receipt: final}
}

func finishReceipt(start Receipt, completed time.Time, cause error) Receipt {
	start.Status = StatusDelivered
	start.CompletedAt = &completed
	if start.StartedAt != nil {
		start.DurationMillis = completed.Sub(*start.StartedAt).Milliseconds()
	}
	if cause != nil {
		start.Status = StatusFailed
		start.Error = sanitizeError(cause)
	}
	return start
}

func sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	const maxRunes = 240
	message := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, err.Error())
	message = strings.Join(strings.Fields(message), " ")
	runes := []rune(message)
	if len(runes) > maxRunes {
		message = string(runes[:maxRunes]) + "..."
	}
	return message
}
