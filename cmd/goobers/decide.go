package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/decider"
	"github.com/goobers/goobers/internal/decisiongate"
	"github.com/goobers/goobers/internal/instance"
)

const decideHelp = "Usage: goobers decide --name <id> --question <text> --state-file <path> [flags] [path]\n\n" +
	"Ask the instance's decision model a yes/no question about a JSON state\n" +
	"document and write a thresholded, deterministic-ish decision to the stage\n" +
	"result file. Code, not the model, owns facts: use this only for semantic\n" +
	"judgments, never for structural validity.\n\n" +
	"Requires decisionGate to be on in the instance config. In shadow mode the\n" +
	"model is still called and recorded but the reported decision is always\n" +
	"\"uncertain\", so a shadowed step never changes a run's outcome.\n\n" +
	"Result keys: decision (yes|no|uncertain|unavailable), probability, mode,\n" +
	"fallback, stateDigest. An uncertain or unavailable answer follows\n" +
	"decisionGate.fallback: fail exits 1; agent and escalate exit 0 so the\n" +
	"workflow can route to an agent or a human.\n" +
	"Flags:\n" +
	"  --name <id>         stable question name (letters, digits, - _ .)\n" +
	"  --question <text>   the yes/no question\n" +
	"  --true <text>       what \"yes\" means (optional)\n" +
	"  --false <text>      what \"no\" means (optional)\n" +
	"  --state-file <path> JSON file the question is about\n" +
	"  --accept <p>        yes at or above this probability (default from config, else 0.9)\n" +
	"  --reject <p>        no at or below this probability (default from config, else 0.1)\n" +
	"  --optional          when the gate is off or unreachable, report \"unavailable\" and exit 0\n" +
	"\nAdvisory CI failure triage (opt-in, shadow first; the existing path is the fallback):\n" +
	"  --ci-log <path>     classify a failed CI log as flaky, real, infra or environment. Code\n" +
	"                      decides known facts first; otherwise the model sees only a redacted,\n" +
	"                      size-bounded excerpt. Result keys: class (unsure keeps today's path;\n" +
	"                      always unsure in shadow mode), predicted, source, digest.\n" +
	"  --ci-baseline <v>   code fact: pr-introduced or shared-baseline-failure\n" +
	"  --run-key <id>      record the prediction (or outcome) under this run identity\n" +
	"  --ci-outcome <c>    record the eventual cause (flaky|real|infra|environment) for --run-key\n" +
	"  --ci-matrix         report the confusion matrix of predictions joined to outcomes\n" +
	"Exit codes: 0 = decision written, 1 = failed, 2 = usage error.\n"

const (
	decideMaxStateBytes = 1 << 20
	decideDefaultAccept = 0.9
	decideDefaultReject = 0.1
)

type decideResult struct {
	Decision    string `json:"decision"`
	Probability string `json:"probability,omitempty"`
	Mode        string `json:"mode"`
	Fallback    string `json:"fallback,omitempty"`
	StateDigest string `json:"stateDigest,omitempty"`
}

func runDecide(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("decide", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "decide")
	name := fs.String("name", "", "stable question name")
	question := fs.String("question", "", "the yes/no question")
	yesText := fs.String("true", "", "what yes means")
	noText := fs.String("false", "", "what no means")
	stateFile := fs.String("state-file", "", "JSON file the question is about")
	accept := fs.Float64("accept", -1, "yes at or above this probability")
	reject := fs.Float64("reject", -1, "no at or below this probability")
	optional := fs.Bool("optional", false, "report unavailable instead of failing when the gate is off or unreachable")
	var ci decideCIArgs
	fs.StringVar(&ci.logFile, "ci-log", "", "triage a failed CI log file instead of asking a yes/no question")
	fs.StringVar(&ci.baseline, "ci-baseline", "", "baseline attribution of the failure (pr-introduced or shared-baseline-failure)")
	fs.StringVar(&ci.runKey, "run-key", "", "run identity the CI triage prediction or outcome is recorded under")
	fs.StringVar(&ci.outcome, "ci-outcome", "", "record the eventual cause (flaky, real, infra, environment) for --run-key")
	fs.BoolVar(&ci.matrix, "ci-matrix", false, "report the shadow confusion matrix of CI triage predictions against outcomes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	root, ok := providerStageRootArg(fs)
	if !ok {
		return 2
	}
	if ci.active() {
		if ci.modes() != 1 || *name != "" || *question != "" || *stateFile != "" {
			pf(stderr, "error: --ci-log, --ci-outcome and --ci-matrix are separate modes and cannot be combined with each other or --name/--question/--state-file\n")
			return 2
		}
		return runDecideCI(ci, root, stdout, stderr, *optional)
	}
	if !validDecideName(*name) || strings.TrimSpace(*question) == "" || *stateFile == "" {
		pf(stderr, "error: --name, --question and --state-file are required (name: letters, digits, - _ . up to 64)\n")
		return 2
	}
	state, err := readDecideState(*stateFile)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	cfg, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile())
	if err != nil {
		pf(stderr, "error: load instance config: %v\n", err)
		return 1
	}
	resultFile := providerInput("resultFile", "decide.json")
	settings := cfg.DecisionGate
	mode := settings.EffectiveMode()
	unavailable := func(reason string) int {
		if !*optional {
			pf(stderr, "error: decide: %s\n", reason)
			return 1
		}
		return writeDecideResult(resultFile, decideResult{Decision: "unavailable", Mode: string(mode)}, stdout, stderr)
	}
	if mode == decisiongate.ModeOff {
		return unavailable("decisionGate is off in the instance config; enable it (shadow or enforce) or pass --optional")
	}

	th := decisiongate.Threshold{Accept: decideDefaultAccept, Reject: decideDefaultReject}
	if t, ok := settings.Gate.Thresholds[*name]; ok {
		th = t
	}
	if *accept >= 0 {
		th.Accept = *accept
	}
	if *reject >= 0 {
		th.Reject = *reject
	}
	// Per-call thresholds live on a private copy so the instance config is never mutated.
	gs := settings
	gs.Gate.Thresholds = map[string]decisiongate.Threshold{}
	for k, v := range settings.Gate.Thresholds {
		gs.Gate.Thresholds[k] = v
	}
	gs.Gate.Thresholds[*name] = th
	if err := gs.Validate(); err != nil {
		pf(stderr, "error: decide: %v\n", err)
		return 2
	}

	var digest string
	gate, err := gs.Resolve(nil, func(e decisiongate.Event) { digest = e.StateDigest })
	if err != nil {
		return unavailable("decision gate unavailable: " + err.Error())
	}

	var criteria *decider.NoulCriteria
	if yes, no := strings.TrimSpace(*yesText), strings.TrimSpace(*noText); yes != "" || no != "" {
		criteria = &decider.NoulCriteria{}
		if yes != "" {
			criteria.True = *yesText
		}
		if no != "" {
			criteria.False = *noText
		}
	}
	q := decider.Noul(*question, criteria)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	outcome, jerr := gate.JudgeNoul(ctx, *name, state, q)
	if jerr != nil {
		pf(stderr, "decide: model call failed; treating as uncertain\n")
	}

	res := decideResult{Decision: string(outcome.Decision), Mode: string(mode), StateDigest: digest}
	if jerr == nil {
		res.Probability = strconv.FormatFloat(outcome.Probability, 'f', 4, 64)
	}
	if mode == decisiongate.ModeShadow {
		res.Decision = string(decisiongate.Uncertain)
	}
	if res.Decision == string(decisiongate.Uncertain) {
		res.Fallback = settings.Fallback
		if settings.Fallback == decisiongate.FallbackFail && mode == decisiongate.ModeEnforce {
			pf(stderr, "error: decide: %q is uncertain and decisionGate.fallback is fail\n", *name)
			return 1
		}
	}
	return writeDecideResult(resultFile, res, stdout, stderr)
}

func writeDecideResult(resultFile string, res decideResult, stdout, stderr io.Writer) int {
	data, err := json.Marshal(res)
	if err != nil {
		pf(stderr, "error: marshal decide result: %v\n", err)
		return 1
	}
	if err := os.WriteFile(resultFile, data, 0o644); err != nil {
		pf(stderr, "error: write %s: %v\n", resultFile, err)
		return 1
	}
	pf(stdout, "decide: %s\n", res.Decision)
	return 0
}

func validDecideName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		if !letter && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.' {
			return false
		}
	}
	return true
}

func readDecideState(path string) (any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open state file: %w", err)
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, decideMaxStateBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read state file: %w", err)
	}
	if len(raw) > decideMaxStateBytes {
		return nil, errors.New("state file exceeds 1 MiB")
	}
	var state any
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, errors.New("state file is not valid JSON")
	}
	return state, nil
}
