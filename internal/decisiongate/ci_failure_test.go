package decisiongate

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/decider"
)

type choiceFake struct {
	choice string
	prob   float64
	err    error
	calls  atomic.Int32
	seen   atomic.Value
}

func (f *choiceFake) Decide(_ context.Context, r decider.Request) (decider.Response, error) {
	f.calls.Add(1)
	f.seen.Store(r.State)
	if f.err != nil {
		return decider.Response{}, f.err
	}
	conf := 0.9
	out := decider.Response{Answers: map[string]decider.Answer{}}
	for id := range r.Questions {
		out.Answers[id] = decider.Answer{Type: decider.KindChoice, Choice: f.choice, Probabilities: map[string]float64{f.choice: f.prob}, Confidence: &conf}
	}
	return out, nil
}

func ciGate(t *testing.T, f *choiceFake) *Gate {
	t.Helper()
	g, err := New(f, Config{Thresholds: map[string]Threshold{CIFailureThresholdKey: DefaultCIFailureThreshold}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func preparedCILog(raw string) CILogExcerpt {
	_, ex := prepareCILog(raw)
	return ex
}

func TestRedactCILogRemovesSecretsAndPersonalData(t *testing.T) {
	raw := "\x1b[31mFAIL\x1b[0m\r\n" +
		"git clone https://user:hunter2pass@example.com/org/repo.git\n" +
		"Authorization: Be" + "arer abcdefghijklmnop123456\n" +
		"API_TOKEN=supersecretvalue1 and password: \"p4ssw0rd!\"\n" +
		"token ghp_" + strings.Repeat("a", 36) + "\n" +
		"contact dev.person@example.com in C:\\Users\\jdoe\\src and /home/jdoe/work\n"
	got := RedactCILog(raw)
	for _, leak := range []string{"hunter2pass", "abcdefghijklmnop123456", "supersecretvalue1", "p4ssw0rd", "ghp_", "dev.person@", "jdoe", "\x1b", "\r"} {
		if strings.Contains(got, leak) {
			t.Errorf("redacted log still contains %q:\n%s", leak, got)
		}
	}
	if !strings.Contains(got, "FAIL") || !strings.Contains(got, "<home>") {
		t.Errorf("redaction dropped useful content:\n%s", got)
	}
}

func TestPrepareCILogBoundsAndKeepsEnds(t *testing.T) {
	raw := "FIRST LINE\n" + strings.Repeat("noise line that is not interesting\n", 2000) + "ghp_" + strings.Repeat("b", 36) + "\nFINAL: --- FAIL: TestX\n"
	ex := preparedCILog(raw)
	if !ex.Truncated || len(ex.Text) > MaxCILogBytes || ex.OriginalBytes != len(raw) {
		t.Fatalf("truncated=%v len=%d original=%d", ex.Truncated, len(ex.Text), ex.OriginalBytes)
	}
	if !strings.HasPrefix(ex.Text, "FIRST LINE") || !strings.HasSuffix(strings.TrimSpace(ex.Text), "--- FAIL: TestX") {
		t.Errorf("excerpt lost the head or the verdict:\n%s", ex.Text[:80])
	}
	if strings.Contains(ex.Text, "ghp_") {
		t.Error("excerpt leaked a token")
	}
	if short := preparedCILog("tiny"); short.Truncated || short.Text != "tiny" {
		t.Errorf("short log changed: %+v", short)
	}
}

func TestPrepareCILogKeepsValidUTF8(t *testing.T) {
	ex := preparedCILog(strings.Repeat("é日本語", 5000))
	if !ex.Truncated || strings.ContainsRune(ex.Text, '\uFFFD') {
		t.Fatalf("invalid UTF-8 in excerpt (truncated=%v)", ex.Truncated)
	}
}

func TestEvaluateCIFailureCodeFactsSkipModel(t *testing.T) {
	f := &choiceFake{choice: "real", prob: 0.99}
	g := ciGate(t, f)
	for name, tc := range map[string]struct {
		log, baseline string
		want          CIFailureClass
	}{
		"infra":    {"step failed: read tcp: ECONNRESET", "", CIInfra},
		"outage":   {"error: 503 Service Unavailable fetching module", "", CIInfra},
		"tool":     {"exec: \"golangci-lint\": executable file not found in $PATH", "", CIEnvironment},
		"baseline": {"--- FAIL: TestX", CIBaselineShared, CIEnvironment},
		"empty":    {"  \n", "", CIUnsure},
	} {
		t.Run(name, func(t *testing.T) {
			res, _, err := g.EvaluateCIFailure(context.Background(), tc.log, tc.baseline)
			if err != nil || res.Class != tc.want {
				t.Fatalf("got %q err=%v, want %q", res.Class, err, tc.want)
			}
			if tc.want != CIUnsure && res.Source != CISourceCode {
				t.Errorf("source = %q, want code", res.Source)
			}
		})
	}
	if f.calls.Load() != 0 {
		t.Fatalf("model called %d times for code-decided logs", f.calls.Load())
	}
}

func TestEvaluateCIFailureModelThresholdsAndFallback(t *testing.T) {
	log := "--- FAIL: TestRace (0.01s)\n    race detected during execution of test"
	for name, tc := range map[string]struct {
		f    *choiceFake
		want CIFailureClass
		src  CIFailureSource
	}{
		"accepted":  {&choiceFake{choice: "flaky", prob: 0.95}, CIFlaky, CISourceModel},
		"unsure":    {&choiceFake{choice: "real", prob: 0.5}, CIUnsure, CISourceNone},
		"not-a-key": {&choiceFake{choice: "banana", prob: 0.99}, CIUnsure, CISourceNone},
	} {
		t.Run(name, func(t *testing.T) {
			res, _, err := ciGate(t, tc.f).EvaluateCIFailure(context.Background(), log, "")
			if err != nil || res.Class != tc.want || res.Source != tc.src {
				t.Fatalf("got %+v err=%v, want %q/%q", res, err, tc.want, tc.src)
			}
		})
	}
	res, _, err := ciGate(t, &choiceFake{err: errors.New("down")}).EvaluateCIFailure(context.Background(), log, "")
	if err == nil || res.Class != CIUnsure {
		t.Fatalf("model error must fall back to unsure, got %+v err=%v", res, err)
	}
}

func TestEvaluateCIFailureSendsOnlyRedactedBoundedLog(t *testing.T) {
	f := &choiceFake{choice: "real", prob: 0.99}
	raw := "password=hunter2hunter2\n" + strings.Repeat("compile noise\n", 3000) + "undefined: foo\n"
	res, _, err := ciGate(t, f).EvaluateCIFailure(context.Background(), raw, CIBaselinePRIntroduced)
	if err != nil || res.Class != CIReal || !res.Truncated {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	sent := f.seen.Load().(ciFailureState)
	if len(sent.Log) > MaxCILogBytes || strings.Contains(sent.Log, "hunter2") || sent.Baseline != CIBaselinePRIntroduced {
		t.Fatalf("model input not redacted/bounded: len=%d baseline=%q", len(sent.Log), sent.Baseline)
	}
	if strings.Contains(res.Digest, "undefined") || !strings.HasPrefix(res.Digest, "sha256:") {
		t.Errorf("digest = %q", res.Digest)
	}
}

func TestJoinCIFailuresMatrix(t *testing.T) {
	pred := func(key string, c CIFailureClass, s CIFailureSource) CIFailureEntry {
		return CIFailureEntry{Kind: CIEntryPrediction, RunKey: key, Class: c, Source: s}
	}
	out := func(key string, c CIFailureClass) CIFailureEntry {
		return CIFailureEntry{Kind: CIEntryOutcome, RunKey: key, Class: c}
	}
	m := JoinCIFailures([]CIFailureEntry{
		pred("a", CIFlaky, CISourceModel), out("a", CIFlaky),
		pred("b", CIFlaky, CISourceModel), out("b", CIReal),
		pred("c", CIReal, CISourceModel), out("c", CIReal),
		pred("d", CIInfra, CISourceCode), out("d", CIInfra),
		pred("e", CIUnsure, CISourceNone), out("e", CIReal),
		pred("f", CIReal, CISourceModel),
		out("g", CIReal),
		pred("h", CIFlaky, CISourceModel), out("h", CIReal), out("h", CIFlaky),
		out("i", CIUnsure),
	})
	if m.ModelJudged() != 4 || m.Model[CIFlaky][CIFlaky] != 2 || m.Model[CIFlaky][CIReal] != 1 || m.Model[CIReal][CIReal] != 1 {
		t.Errorf("model matrix = %v", m.Model)
	}
	if m.Code[CIInfra][CIInfra] != 1 || m.Unsure != 1 || m.Pending != 1 || m.Orphans != 1 || m.WronglySkipped != 1 {
		t.Errorf("matrix = %+v", m)
	}
	if _, ok := m.ModelAccuracy(5); ok {
		t.Error("accuracy reported below the minimum sample")
	}
	if acc, ok := m.ModelAccuracy(4); !ok || acc != 0.75 {
		t.Errorf("accuracy = %v %v, want 0.75", acc, ok)
	}
}
