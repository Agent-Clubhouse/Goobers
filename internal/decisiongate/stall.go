package decisiongate

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/internal/decider"
)

// StallThresholdKey is the threshold key for the semantic no-progress
// question.
const StallThresholdKey = "transcript_no_progress"

// DefaultStallThreshold keeps ambiguous windows unflagged.
var DefaultStallThreshold = Threshold{Accept: 0.9, Reject: 0.1}

// Stall detection tuning. Windows are already bounded and redacted by the
// harness; these only decide when code or the model has enough to go on.
const (
	// minStallLines is the smallest window worth judging at all.
	minStallLines = 8
	// repeatCycles is how many times a short line cycle must repeat back to
	// back to count as a loop.
	repeatCycles = 4
	// maxStallCycle is the longest line cycle searched for.
	maxStallCycle = 3
	// maxStallModelBytes bounds what a model sees.
	maxStallModelBytes = 8 << 10
	// maxStallModelCalls bounds model spend per stream.
	maxStallModelCalls = 8
	// maxStallStreams bounds tracked streams so a runaway producer cannot
	// grow memory.
	maxStallStreams = 64
)

var (
	stallDigits = regexp.MustCompile(`\d+`)
	// stallFileChange recognises transcript lines that show the workspace
	// changing. It is a heuristic fact, not a parser for any one harness.
	stallFileChange = regexp.MustCompile(`(?i)(^diff --git|^\+\+\+ |\b(wrote|created|updated|modified|edited|deleted) (file|\S+\.\w+)|\b(apply_patch|str_replace|write_file|create_file|edit_file)\b|git (add|commit)\b)`)
)

var stallPrompt = decider.Noul(
	"Does this tail of an agent session show it making no meaningful progress toward its task?",
	&decider.NoulCriteria{
		True:  "The agent keeps retrying or re-reading the same things, or circles without producing new results or changes.",
		False: "The agent is exploring, editing, running new commands, or otherwise moving the task forward.",
	},
)

// StallFacts are code-decided properties of a transcript window.
type StallFacts struct {
	Lines int
	// LoopCycles is how many times the best short line cycle repeats at the
	// end of the window (0 or 1 means no loop).
	LoopCycles int
	// FileChanged reports whether any line shows the workspace changing.
	FileChanged bool
}

// StallBasis says who decided a stall verdict.
type StallBasis string

// Bases.
const (
	StallBasisCode  StallBasis = "code"
	StallBasisModel StallBasis = "model"
	StallBasisNone  StallBasis = "none"
)

// StallVerdict is the advisory reading of a window. StallUnsure means the
// caller changes nothing.
type StallVerdict string

// Verdicts.
const (
	StallDetected    StallVerdict = "stalled"
	StallProgressing StallVerdict = "progressing"
	StallUnsure      StallVerdict = "unsure"
)

func normalizeStallLine(line string) string {
	return stallDigits.ReplaceAllString(strings.TrimSpace(line), "#")
}

// StallFactsOf computes the cheap deterministic facts for a window: how many
// back-to-back repeats of a short line cycle end the window, and whether any
// line shows a file change. No model is involved.
func StallFactsOf(window string) StallFacts {
	var lines []string
	f := StallFacts{}
	for _, raw := range strings.Split(window, "\n") {
		line := normalizeStallLine(raw)
		if line == "" {
			continue
		}
		lines = append(lines, line)
		if stallFileChange.MatchString(strings.TrimSpace(raw)) {
			f.FileChanged = true
		}
	}
	f.Lines = len(lines)
	for period := 1; period <= maxStallCycle; period++ {
		if cycles := trailingCycles(lines, period); cycles > f.LoopCycles {
			f.LoopCycles = cycles
		}
	}
	return f
}

// trailingCycles counts how many times the last period lines repeat
// consecutively at the end of lines.
func trailingCycles(lines []string, period int) int {
	if len(lines) < period {
		return 0
	}
	unit := lines[len(lines)-period:]
	n := 1
	for end := len(lines) - period; end-period >= 0; end -= period {
		same := true
		for i := range period {
			if lines[end-period+i] != unit[i] {
				same = false
				break
			}
		}
		if !same {
			break
		}
		n++
	}
	return n
}

// DecideStallByCode returns a verdict when facts alone settle it: a repeating
// loop with no file change is a stall. Anything else needs the model or stays
// unsure.
func DecideStallByCode(f StallFacts) (StallVerdict, bool) {
	if f.LoopCycles >= repeatCycles && !f.FileChanged {
		return StallDetected, true
	}
	return StallUnsure, false
}

// EvaluateStall reads one redacted window. Repetition without file changes is
// decided in code; a short window is left unsure; otherwise the model judges
// only the semantic no-progress question. Uncertain answers and errors are
// StallUnsure so nothing downstream changes.
func (g *Gate) EvaluateStall(ctx context.Context, window string) (StallVerdict, StallBasis, Outcome, error) {
	facts := StallFactsOf(window)
	if v, ok := DecideStallByCode(facts); ok {
		return v, StallBasisCode, Outcome{Name: StallThresholdKey}, nil
	}
	if facts.Lines < minStallLines {
		return StallUnsure, StallBasisNone, Outcome{Name: StallThresholdKey}, nil
	}
	if len(window) > maxStallModelBytes {
		window = window[len(window)-maxStallModelBytes:]
	}
	o, err := g.JudgeNoul(ctx, StallThresholdKey, window, stallPrompt)
	if err != nil {
		return StallUnsure, StallBasisModel, o, err
	}
	switch o.Decision {
	case Yes:
		return StallDetected, StallBasisModel, o, nil
	case No:
		return StallProgressing, StallBasisModel, o, nil
	}
	return StallUnsure, StallBasisModel, o, nil
}

// StallRecord is one window's shadow reading. It holds no transcript text.
type StallRecord struct {
	Stream      string
	Window      int
	Verdict     StallVerdict
	Basis       StallBasis
	Facts       StallFacts
	Probability float64
	Err         error
}

// StallOutcome closes a stream: what was flagged, against whether the stage
// then hit its budget. FlaggedWindow is 0 when never flagged.
type StallOutcome struct {
	Stream        string
	Windows       int
	FlaggedWindow int
	BudgetHit     bool
}

// StallTally compares flags with budget exhaustion across finished streams.
type StallTally struct {
	Streams int
	// Flagged streams split by what followed.
	FlaggedBudgetHit, FlaggedCompleted int
	// MissedBudgetHit is budget-hit streams never flagged.
	MissedBudgetHit int
}

// Add folds one finished stream in.
func (t *StallTally) Add(o StallOutcome) {
	t.Streams++
	flagged := o.FlaggedWindow > 0
	switch {
	case flagged && o.BudgetHit:
		t.FlaggedBudgetHit++
	case flagged:
		t.FlaggedCompleted++
	case o.BudgetHit:
		t.MissedBudgetHit++
	}
}

// Precision is the share of flagged streams that went on to hit the budget.
// It refuses a figure below minSample flagged streams.
func (t StallTally) Precision(minSample int) (float64, bool) {
	flagged := t.FlaggedBudgetHit + t.FlaggedCompleted
	if flagged == 0 || flagged < minSample {
		return 0, false
	}
	return float64(t.FlaggedBudgetHit) / float64(flagged), true
}

// StallObserver scores transcript windows in the background without ever
// blocking the stage. Code facts are computed on the calling goroutine and
// settle loops immediately; only windows that need the model queue, and each
// stream keeps just its latest pending window, so a burst cannot starve the
// window that matters.
type StallObserver struct {
	Gate    *Gate
	Sample  float64
	Timeout time.Duration
	Record  func(StallRecord)
	Finish  func(StallOutcome)

	sem     chan struct{}
	mu      sync.Mutex
	streams map[string]*stallStream
	dropped int64
	wg      sync.WaitGroup
}

type stallStream struct {
	windows, flagged, modelCalls int
	pending                      *string
	pendingN                     int
	inflight                     bool
	finished                     bool
}

// NewStallObserver builds an observer allowing at most workers concurrent
// model calls.
func NewStallObserver(g *Gate, sample float64, workers int, record func(StallRecord), finish func(StallOutcome)) *StallObserver {
	if workers <= 0 {
		workers = 2
	}
	return &StallObserver{
		Gate: g, Sample: sample, Timeout: 8 * time.Second, Record: record, Finish: finish,
		sem: make(chan struct{}, workers), streams: map[string]*stallStream{},
	}
}

// Dropped counts windows skipped because the stream table was full.
func (o *StallObserver) Dropped() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.dropped
}

// Wait blocks until in-flight scorings finish (tests and shutdown).
func (o *StallObserver) Wait() { o.wg.Wait() }

// ObserveWindow records the latest redacted window of a stream. stream must be
// a stable per-stage key.
func (o *StallObserver) ObserveWindow(stream, window string) {
	if o == nil || o.Gate == nil || window == "" || !Sampled(stream, o.Sample) {
		return
	}
	o.mu.Lock()
	st := o.streams[stream]
	if st == nil {
		if len(o.streams) >= maxStallStreams {
			o.dropped++
			o.mu.Unlock()
			return
		}
		st = &stallStream{}
		o.streams[stream] = st
	}
	if st.finished {
		o.mu.Unlock()
		return
	}
	st.windows++
	n := st.windows
	facts := StallFactsOf(window)
	if v, ok := DecideStallByCode(facts); ok {
		if st.flagged == 0 {
			st.flagged = n
		}
		o.mu.Unlock()
		o.record(StallRecord{Stream: stream, Window: n, Verdict: v, Basis: StallBasisCode, Facts: facts})
		return
	}
	if facts.Lines < minStallLines || st.modelCalls >= maxStallModelCalls {
		o.mu.Unlock()
		return
	}
	st.pending, st.pendingN = &window, n
	start := !st.inflight
	if start {
		select {
		case o.sem <- struct{}{}:
			st.inflight = true
		default:
			start = false
		}
	}
	if start {
		o.wg.Add(1)
	}
	o.mu.Unlock()
	if start {
		go o.drain(stream, st)
	}
}

// drain scores a stream's latest pending window until none remain.
func (o *StallObserver) drain(stream string, st *stallStream) {
	defer o.wg.Done()
	defer func() { <-o.sem }()
	for {
		o.mu.Lock()
		if st.pending == nil || st.finished || st.modelCalls >= maxStallModelCalls {
			st.pending, st.inflight = nil, false
			o.mu.Unlock()
			return
		}
		window, n := *st.pending, st.pendingN
		st.pending = nil
		st.modelCalls++
		o.mu.Unlock()

		rec := o.score(stream, n, window)
		o.mu.Lock()
		if rec.Verdict == StallDetected && st.flagged == 0 {
			st.flagged = n
		}
		o.mu.Unlock()
		o.record(rec)
	}
}

func (o *StallObserver) score(stream string, n int, window string) (rec StallRecord) {
	rec = StallRecord{Stream: stream, Window: n, Verdict: StallUnsure, Basis: StallBasisModel, Facts: StallFactsOf(window)}
	defer func() {
		if recover() != nil {
			rec.Verdict = StallUnsure
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
	defer cancel()
	v, basis, out, err := o.Gate.EvaluateStall(ctx, window)
	rec.Verdict, rec.Basis, rec.Probability, rec.Err = v, basis, out.Probability, err
	return rec
}

func (o *StallObserver) record(r StallRecord) {
	if o.Record != nil {
		defer func() { _ = recover() }()
		o.Record(r)
	}
}

// FinishStream closes a stream with the budget-hit label and reports the
// outcome. It is idempotent per stream. A model call still in flight may
// record after the outcome; its flag then does not count.
func (o *StallObserver) FinishStream(stream string, budgetHit bool) {
	if o == nil {
		return
	}
	o.mu.Lock()
	st := o.streams[stream]
	if st == nil || st.finished {
		o.mu.Unlock()
		return
	}
	st.finished = true
	out := StallOutcome{Stream: stream, Windows: st.windows, FlaggedWindow: st.flagged, BudgetHit: budgetHit}
	delete(o.streams, stream)
	o.mu.Unlock()
	if o.Finish != nil {
		defer func() { _ = recover() }()
		o.Finish(out)
	}
}

// StreamsWithPrefix returns the open streams whose key starts with prefix, so
// a stage end can close every stream the stage opened.
func (o *StallObserver) StreamsWithPrefix(prefix string) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []string
	for k := range o.streams {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}
