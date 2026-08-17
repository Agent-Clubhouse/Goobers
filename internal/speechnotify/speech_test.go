package speechnotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	platformlock "github.com/goobers/goobers/internal/platform/lock"
)

type memoryRecorder struct {
	mu       sync.Mutex
	receipts []Receipt
	err      error
}

type FakeSynthesizer struct {
	Report       Preflight
	PreflightErr error
	Err          error

	mu         sync.Mutex
	utterances []string
}

func (*FakeSynthesizer) Name() string { return "fake" }

func (f *FakeSynthesizer) Preflight(context.Context, Config) (Preflight, error) {
	report := f.Report
	if report.Engine == "" {
		report = Preflight{
			Engine:            "fake",
			Executable:        "in-process",
			Voice:             "fake",
			Language:          "und",
			Rate:              DefaultRate,
			AudioPrerequisite: "none",
			AudioAvailable:    true,
		}
	}
	return report, f.PreflightErr
}

func (f *FakeSynthesizer) Synthesize(ctx context.Context, _ Config, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	f.utterances = append(f.utterances, text)
	f.mu.Unlock()
	return f.Err
}

func (f *FakeSynthesizer) Utterances() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.utterances...)
}

func (r *memoryRecorder) Record(_ context.Context, receipt Receipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.receipts = append(r.receipts, receipt)
	return r.err
}

func (r *memoryRecorder) snapshot() []Receipt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Receipt(nil), r.receipts...)
}

func TestConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		config  Config
		wantErr string
	}{
		{name: "defaults", config: Config{}},
		{name: "configured", config: Config{Engine: "say", Voice: "Samantha", Language: "en-US", Rate: 210, Timeout: "8s"}},
		{name: "engine", config: Config{Engine: "shell"}, wantErr: "not supported"},
		{name: "voice option", config: Config{Voice: "--output"}, wantErr: "must not begin"},
		{name: "language", config: Config{Language: "english!"}, wantErr: "BCP 47"},
		{name: "slow rate", config: Config{Rate: 79}, wantErr: "between 80 and 450"},
		{name: "fast rate", config: Config{Rate: 451}, wantErr: "between 80 and 450"},
		{name: "short timeout", config: Config{Timeout: "100ms"}, wantErr: "between 1s"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.config.Validate()
			if test.wantErr == "" && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("Validate error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestFakeSynthesizerPreservesExactTextAndRecordsReceipts(t *testing.T) {
	fake := &FakeSynthesizer{}
	recorder := &memoryRecorder{}
	sink, err := New(Config{}, fake, recorder)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	text := "CPU is 91.2% — literal $(touch /tmp/nope); newline\n2 ms"
	receipt, err := sink.Deliver(context.Background(), Request{NotificationID: "incident-1", Text: text})
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if receipt.Status != StatusDelivered || receipt.Engine != "fake" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if got := fake.Utterances(); len(got) != 1 || got[0] != text {
		t.Fatalf("utterances = %#v, want exact text", got)
	}
	receipts := recorder.snapshot()
	if len(receipts) != 2 || receipts[0].Status != StatusStarted || receipts[1].Status != StatusDelivered {
		t.Fatalf("receipts = %+v", receipts)
	}
}

type blockingSynthesizer struct {
	mu        sync.Mutex
	active    int
	maxActive int
	started   chan string
	release   chan struct{}
	order     []string
}

func (*blockingSynthesizer) Name() string { return "blocking" }

func (*blockingSynthesizer) Preflight(context.Context, Config) (Preflight, error) {
	return Preflight{Engine: "blocking", AudioAvailable: true}, nil
}

func (s *blockingSynthesizer) Synthesize(ctx context.Context, _ Config, text string) error {
	s.mu.Lock()
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.order = append(s.order, text)
	s.mu.Unlock()
	s.started <- text
	select {
	case <-s.release:
	case <-ctx.Done():
		s.mu.Lock()
		s.active--
		s.mu.Unlock()
		return ctx.Err()
	}
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return nil
}

func TestSinkSerializesAcceptedDeliveries(t *testing.T) {
	synth := &blockingSynthesizer{
		started: make(chan string, 2),
		release: make(chan struct{}, 2),
	}
	sink, err := New(Config{}, synth, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	results := make(chan error, 2)
	go func() {
		_, err := sink.Deliver(context.Background(), Request{NotificationID: "one", Text: "one"})
		results <- err
	}()
	if got := <-synth.started; got != "one" {
		t.Fatalf("first started = %q", got)
	}
	go func() {
		_, err := sink.Deliver(context.Background(), Request{NotificationID: "two", Text: "two"})
		results <- err
	}()
	select {
	case got := <-synth.started:
		t.Fatalf("overlapping delivery started: %q", got)
	case <-time.After(20 * time.Millisecond):
	}
	synth.release <- struct{}{}
	if got := <-synth.started; got != "two" {
		t.Fatalf("second started = %q", got)
	}
	synth.release <- struct{}{}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("Deliver: %v", err)
		}
	}
	synth.mu.Lock()
	defer synth.mu.Unlock()
	if synth.maxActive != 1 || strings.Join(synth.order, ",") != "one,two" {
		t.Fatalf("max active = %d, order = %v", synth.maxActive, synth.order)
	}
}

func TestSinkTimeoutProducesSanitizedFailureReceipt(t *testing.T) {
	synth := &blockingSynthesizer{
		started: make(chan string, 1),
		release: make(chan struct{}),
	}
	recorder := &memoryRecorder{}
	sink, err := New(Config{Timeout: "1s"}, synth, recorder)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	receipt, err := sink.Deliver(ctx, Request{NotificationID: "timeout", Text: "wait"})
	if err == nil || receipt.Status != StatusFailed || receipt.Error != "context deadline exceeded" {
		t.Fatalf("Deliver = (%+v, %v)", receipt, err)
	}
	receipts := recorder.snapshot()
	if len(receipts) != 2 || receipts[1].Status != StatusFailed {
		t.Fatalf("receipts = %+v", receipts)
	}
}

func TestSinkQueuedCancellationReturnsWithoutOverlap(t *testing.T) {
	synth := &blockingSynthesizer{
		started: make(chan string, 2),
		release: make(chan struct{}, 1),
	}
	sink, err := New(Config{}, synth, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := sink.Deliver(context.Background(), Request{NotificationID: "active", Text: "active"})
		firstDone <- err
	}()
	if got := <-synth.started; got != "active" {
		t.Fatalf("first started = %q", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan deliveryResult, 1)
	go func() {
		receipt, err := sink.Deliver(ctx, Request{NotificationID: "cancelled", Text: "cancelled"})
		secondDone <- deliveryResult{receipt: receipt, err: err}
	}()
	cancel()
	var result deliveryResult
	select {
	case result = <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("queued cancellation did not return promptly")
	}
	if result.err == nil || result.receipt.Status != StatusFailed ||
		result.receipt.Error != "context canceled" {
		t.Fatalf("cancelled Deliver = (%+v, %v)", result.receipt, result.err)
	}
	synth.release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatalf("first Deliver: %v", err)
	}
	synth.mu.Lock()
	defer synth.mu.Unlock()
	if synth.maxActive != 1 || strings.Join(synth.order, ",") != "active" {
		t.Fatalf("max active = %d, order = %v", synth.maxActive, synth.order)
	}
}

func TestSinkRejectsPreCancelledRequestBeforeAdmission(t *testing.T) {
	fake := &FakeSynthesizer{}
	recorder := &memoryRecorder{}
	sink, err := New(Config{}, fake, recorder)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	receipt, err := sink.Deliver(ctx, Request{NotificationID: "cancelled", Text: "must not start"})
	if err == nil || receipt.Status != StatusFailed || receipt.Error != "context canceled" {
		t.Fatalf("Deliver = (%+v, %v)", receipt, err)
	}
	if got := fake.Utterances(); len(got) != 0 {
		t.Fatalf("utterances = %#v, want none", got)
	}
	receipts := recorder.snapshot()
	if len(receipts) != 1 || receipts[0].Status != StatusFailed {
		t.Fatalf("receipts = %+v, want one failed receipt", receipts)
	}
}

func TestFileRecorderWritesJSONLWithoutText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler", "speech-receipts.jsonl")
	recorder := NewFileRecorder(path)
	receipt := Receipt{
		Version:        receiptVersion,
		NotificationID: "run-123",
		Engine:         EngineSay,
		Status:         StatusDelivered,
		DurationMillis: 1000,
	}
	started := time.Unix(1, 0).UTC()
	completed := time.Unix(2, 0).UTC()
	receipt.StartedAt = &started
	receipt.CompletedAt = &completed
	if err := recorder.Record(context.Background(), receipt); err != nil {
		t.Fatalf("Record: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got Receipt
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.NotificationID != receipt.NotificationID || strings.Contains(string(raw), "speech text") {
		t.Fatalf("receipt = %s", raw)
	}
}

func TestFileRecorderBoundsReceiptLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler", ReceiptFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, receiptFileMaxSize), 0o600); err != nil {
		t.Fatal(err)
	}

	recorder := NewFileRecorder(path)
	receipt := Receipt{
		Version:        receiptVersion,
		NotificationID: "newest",
		Engine:         EngineSay,
		Status:         StatusDelivered,
	}
	if err := recorder.Record(context.Background(), receipt); err != nil {
		t.Fatalf("Record: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > receiptFileMaxSize {
		t.Fatalf("receipt log size = %d, want at most %d", len(raw), receiptFileMaxSize)
	}
	var got Receipt
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode retained receipt: %v", err)
	}
	if got.NotificationID != receipt.NotificationID {
		t.Fatalf("retained notification ID = %q, want %q", got.NotificationID, receipt.NotificationID)
	}
}

func TestFileRecorderBoundsReceiptLogAcrossConcurrentRecorders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scheduler", ReceiptFileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, receiptFileMaxSize), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := platformlock.TryAcquire(path + receiptLockSuffix)
	if err != nil {
		t.Fatalf("hold receipt lock: %v", err)
	}

	const recorderCount = 8
	start := make(chan struct{})
	results := make(chan error, recorderCount)
	for i := range recorderCount {
		recorder := NewFileRecorder(path)
		go func() {
			<-start
			results <- recorder.Record(context.Background(), Receipt{
				Version:        receiptVersion,
				NotificationID: strings.Repeat("x", 4096) + fmt.Sprint(i),
				Engine:         EngineSay,
				Status:         StatusDelivered,
			})
		}()
	}
	close(start)
	select {
	case err := <-results:
		t.Fatalf("Record completed without acquiring shared lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := held.Release(); err != nil {
		t.Fatalf("release receipt lock: %v", err)
	}
	for range recorderCount {
		if err := <-results; err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > receiptFileMaxSize {
		t.Fatalf("receipt log size = %d, want at most %d", len(raw), receiptFileMaxSize)
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(string(raw), "\n"), "\n") {
		var receipt Receipt
		if err := json.Unmarshal([]byte(line), &receipt); err != nil {
			t.Fatalf("decode receipt: %v", err)
		}
	}
}

func TestFileRecorderRejectsReceiptLargerThanLogBound(t *testing.T) {
	path := filepath.Join(t.TempDir(), ReceiptFileName)
	recorder := NewFileRecorder(path)
	receipt := Receipt{
		Version:        receiptVersion,
		NotificationID: strings.Repeat("x", receiptFileMaxSize),
		Engine:         EngineSay,
		Status:         StatusDelivered,
	}
	if err := recorder.Record(context.Background(), receipt); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Record error = %v, want size limit", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("receipt log size = %d, want 0", info.Size())
	}
}

func TestFailedSynthesizerDoesNotReportDelivery(t *testing.T) {
	fake := &FakeSynthesizer{Err: errors.New("device\nsecret\x1b detail")}
	sink, err := New(Config{}, fake, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	receipt, err := sink.Deliver(context.Background(), Request{NotificationID: "failed", Text: "alert"})
	if err == nil || receipt.Status != StatusFailed || receipt.Error != "device secret detail" {
		t.Fatalf("Deliver = (%+v, %v)", receipt, err)
	}
}
