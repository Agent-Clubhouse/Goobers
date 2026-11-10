package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/instance"
)

// maxDecideCILogBytes caps how much of a CI log file is read; the gate bounds
// what is sent to the model far below this.
const maxDecideCILogBytes = 4 << 20

// decideCIMinSample is the fewest resolved model decisions before the matrix
// reports an accuracy figure.
const decideCIMinSample = 30

type decideCIArgs struct {
	logFile, baseline, runKey, outcome string
	matrix                             bool
}

func (a decideCIArgs) active() bool { return a.modes() > 0 }

func (a decideCIArgs) modes() int {
	n := 0
	for _, on := range []bool{a.logFile != "", a.outcome != "", a.matrix} {
		if on {
			n++
		}
	}
	return n
}

func decideCILedgerPath(root string) string {
	return filepath.Join(instance.NewLayout(root).SchedulerDir(), "ci-failure-triage", "ledger.jsonl")
}

// runDecideCI is the advisory CI failure triage mode of decide: classify a
// failed CI log, record the eventual outcome, or print the shadow confusion
// matrix. It never changes a run: shadow mode reports "unsure".
func runDecideCI(a decideCIArgs, root string, stdout, stderr io.Writer, optional bool) int {
	resultFile := providerInput("resultFile", "decide.json")
	switch {
	case a.outcome != "":
		return decideCIOutcome(a, root, resultFile, stdout, stderr)
	case a.matrix:
		return decideCIMatrix(root, resultFile, stdout, stderr)
	}
	return decideCIClassify(a, root, resultFile, stdout, stderr, optional)
}

func decideCIClassify(a decideCIArgs, root, resultFile string, stdout, stderr io.Writer, optional bool) int {
	raw, err := readDecideCILog(a.logFile)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		pf(stderr, "error: load instance config: %v\n", err)
		return 1
	}
	settings := cfg.DecisionGate
	mode := settings.EffectiveMode()
	unavailable := func(reason string) int {
		if !optional {
			pf(stderr, "error: decide: %s\n", reason)
			return 1
		}
		return writeDecideCIResult(resultFile, map[string]string{"class": string(decisiongate.CIUnsure), "mode": string(mode), "unavailable": "true"}, stdout, stderr)
	}
	if mode == decisiongate.ModeOff {
		return unavailable("decisionGate is off in the instance config; enable it (shadow or enforce) or pass --optional")
	}
	if err := settings.Validate(); err != nil {
		pf(stderr, "error: decide: %v\n", err)
		return 2
	}
	gate, err := settings.Resolve(nil, nil)
	if err != nil {
		return unavailable("decision gate unavailable: " + err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, outcome, jerr := gate.EvaluateCIFailure(ctx, raw, a.baseline)
	if jerr != nil {
		pf(stderr, "decide: model call failed; treating as unsure\n")
	}
	if a.runKey != "" {
		entry := decisiongate.CIFailureEntry{Kind: decisiongate.CIEntryPrediction, RunKey: a.runKey, Class: res.Class, Source: res.Source, Probability: outcome.Probability, Digest: res.Digest}
		if err := appendCILedger(decideCILedgerPath(root), entry); err != nil {
			pf(stderr, "decide: record prediction: %v\n", err)
		}
	}
	out := map[string]string{
		"class": string(res.Class), "predicted": string(res.Class), "source": string(res.Source),
		"mode": string(mode), "digest": res.Digest,
	}
	if res.Reason != "" {
		out["reason"] = res.Reason
	}
	if jerr == nil && res.Source == decisiongate.CISourceModel {
		out["probability"] = strconv.FormatFloat(res.Probability, 'f', 4, 64)
	}
	if mode == decisiongate.ModeShadow {
		out["class"] = string(decisiongate.CIUnsure)
	}
	if out["class"] == string(decisiongate.CIUnsure) {
		out["fallback"] = settings.Fallback
	}
	return writeDecideCIResult(resultFile, out, stdout, stderr)
}

func decideCIOutcome(a decideCIArgs, root, resultFile string, stdout, stderr io.Writer) int {
	class := decisiongate.CIFailureClass(a.outcome)
	if a.runKey == "" || !decisiongate.ValidEventualCIClass(class) {
		pf(stderr, "error: --ci-outcome needs --run-key and one of flaky, real, infra, environment\n")
		return 2
	}
	entry := decisiongate.CIFailureEntry{Kind: decisiongate.CIEntryOutcome, RunKey: a.runKey, Class: class}
	if err := appendCILedger(decideCILedgerPath(root), entry); err != nil {
		pf(stderr, "error: record outcome: %v\n", err)
		return 1
	}
	return writeDecideCIResult(resultFile, map[string]string{"recorded": "outcome", "runKey": a.runKey, "eventual": string(class)}, stdout, stderr)
}

func decideCIMatrix(root, resultFile string, stdout, stderr io.Writer) int {
	entries, err := readCILedger(decideCILedgerPath(root))
	if err != nil {
		pf(stderr, "error: read ledger: %v\n", err)
		return 1
	}
	m := decisiongate.JoinCIFailures(entries)
	out := map[string]string{
		"modelJudged": strconv.Itoa(m.ModelJudged()), "pending": strconv.Itoa(m.Pending), "orphans": strconv.Itoa(m.Orphans),
		"unsure": strconv.Itoa(m.Unsure), "wronglySkipped": strconv.Itoa(m.WronglySkipped),
		"minSample": strconv.Itoa(decideCIMinSample),
	}
	if acc, ok := m.ModelAccuracy(decideCIMinSample); ok {
		out["modelAccuracy"] = strconv.FormatFloat(acc, 'f', 4, 64)
	}
	for _, c := range []struct {
		prefix string
		cells  decisiongate.CIConfusion
	}{{"model", m.Model}, {"code", m.Code}} {
		for pred, row := range c.cells {
			for eventual, n := range row {
				out[fmt.Sprintf("%s.%s>%s", c.prefix, pred, eventual)] = strconv.Itoa(n)
			}
		}
	}
	return writeDecideCIResult(resultFile, out, stdout, stderr)
}

func writeDecideCIResult(resultFile string, out map[string]string, stdout, stderr io.Writer) int {
	data, err := json.Marshal(out)
	if err != nil {
		pf(stderr, "error: marshal decide result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "decide: %s\n", firstNonEmpty(out["class"], out["recorded"], "matrix"))
	return 0
}

// readDecideCILog reads at most maxDecideCILogBytes from the end of the file,
// where CI tools print their verdict.
func readDecideCILog(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open ci log: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat ci log: %w", err)
	}
	if info.Size() > maxDecideCILogBytes {
		if _, err := f.Seek(info.Size()-maxDecideCILogBytes, io.SeekStart); err != nil {
			return "", fmt.Errorf("seek ci log: %w", err)
		}
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxDecideCILogBytes))
	if err != nil {
		return "", fmt.Errorf("read ci log: %w", err)
	}
	return string(raw), nil
}

func appendCILedger(path string, e decisiongate.CIFailureEntry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func readCILedger(path string) ([]decisiongate.CIFailureEntry, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []decisiongate.CIFailureEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e decisiongate.CIFailureEntry
		if strings.TrimSpace(sc.Text()) != "" && json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}
