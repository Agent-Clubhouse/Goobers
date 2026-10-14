// Command reviewereval measures the merge-review agent's recall and
// precision on real bugs this repository already fixed (#7112).
//
// It traces bug fixes back to the commits that introduced them with
// refactoring-aware SZZ (R-SZZ), replays merge-review on each offending change
// reconstructed in isolation, and scores the findings against the lines the
// fix later touched. Run from the repository root:
//
//	go run ./test/reviewereval build -range=A..B -version=v1 -out=dataset.jsonl
//	go run ./test/reviewereval replay -dataset=dataset.jsonl -variant=baseline \
//	    -reviewer-cmd="<adapter>" -allow-real-reviewer -max-cases=20 -budget-usd=10 -report=baseline.json
//	go run ./test/reviewereval compare -baseline=baseline.json -candidate=candidate.json
//	go run ./test/reviewereval publish -report=baseline.json -instance=<instance-root>
//
// build only reads the repository. replay never posts anywhere: the reviewer
// runs in a fresh repository with no remote and no forge credentials. compare
// exits 1 when the candidate regresses; -publish records the comparison on an
// instance so telemetry read surfaces report it.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/goobers/goobers/internal/readservice"
)

const usage = "usage: reviewereval build|replay|compare|publish [flags]"

func main() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	commands := map[string]func(context.Context, []string, io.Writer, io.Writer) int{
		"build": runBuild, "replay": runReplay, "compare": runCompare, "publish": runPublish,
	}
	command, ok := commands[args[0]]
	if !ok {
		_, _ = fmt.Fprintln(stderr, usage)
		return 2
	}
	return command(ctx, args[1:], stdout, stderr)
}

func runBuild(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("build", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repo := flags.String("repo", ".", "repository to read history from (never written)")
	rangeSpec := flags.String("range", "", "commit range to scan for fixes, e.g. v1.0..main")
	version := flags.String("version", "", "dataset version recorded in its header")
	out := flags.String("out", "", "write the JSONL dataset here (default stdout)")
	bugIssues := flags.String("bug-issues", "", "file of issue numbers labeled bug or flake, one per line")
	listed := flags.String("fixes", "", "file of fix commit SHAs, one per line")
	labels := flags.String("hand-labels", "", "JSONL file of hand labels: fixCommit, introducingCommit, labeledBy, labeledAt, note")
	noConventional := flags.Bool("no-conventional", false, "do not treat conventional fix: commits as fixes")
	maxFixes := flags.Int("max-fixes", defaultMaxFixes, "stop after this many fixes")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *rangeSpec == "" || *version == "" {
		_, _ = fmt.Fprintln(stderr, "reviewereval build: -range and -version are required")
		return 2
	}
	options := buildOptions{Range: *rangeSpec, Version: *version, NoConventional: *noConventional, MaxFixes: *maxFixes, BugIssues: map[int]bool{}}
	if err := loadBuildInputs(&options, *bugIssues, *listed, *labels); err != nil {
		_, _ = fmt.Fprintf(stderr, "reviewereval build: %v\n", err)
		return 2
	}
	set, err := newBuilder(*repo).build(ctx, options)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reviewereval build: %v\n", err)
		return 1
	}
	if err := writeOutput(*out, stdout, func(w io.Writer) error { return writeDataset(w, set) }); err != nil {
		_, _ = fmt.Fprintf(stderr, "reviewereval build: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stderr, "reviewer eval dataset %s: %d fixes, %d cases, filtered %v, dropped %v\n",
		set.Header.Version, set.Header.FixesFound, set.Header.CaseCount, set.Header.Filtered, set.Header.Dropped)
	return 0
}

func loadBuildInputs(options *buildOptions, bugIssues, listed, labels string) error {
	issues, err := readLines(bugIssues)
	if err != nil {
		return err
	}
	for _, line := range issues {
		number, err := strconv.Atoi(strings.TrimPrefix(line, "#"))
		if err != nil {
			return fmt.Errorf("bug issue %q is not a number", line)
		}
		options.BugIssues[number] = true
	}
	if options.ListedFixes, err = readLines(listed); err != nil {
		return err
	}
	labelLines, err := readLines(labels)
	if err != nil {
		return err
	}
	for _, line := range labelLines {
		var label handLabel
		if err := json.Unmarshal([]byte(line), &label); err != nil {
			return fmt.Errorf("decode hand label %q: %w", line, err)
		}
		if label.FixCommit == "" {
			return fmt.Errorf("hand label %q needs a fixCommit", line)
		}
		options.HandLabels = append(options.HandLabels, label)
	}
	return nil
}

// readLines returns a file's non-blank lines, ignoring # comments that start
// a line followed by a space. An empty path reads nothing.
func readLines(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var lines []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" && !strings.HasPrefix(line, "# ") {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

func runReplay(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("replay", flag.ContinueOnError)
	flags.SetOutput(stderr)
	datasetPath := flags.String("dataset", "", "frozen JSONL dataset to replay")
	repo := flags.String("repo", ".", "repository the dataset was built from (only read)")
	options := replayOptions{}
	flags.StringVar(&options.Split, "split", splitEval, "dataset split to replay: eval or train")
	flags.StringVar(&options.Variant, "variant", "", "name of the reviewer variant under test")
	flags.IntVar(&options.MaxCases, "max-cases", 20, fmt.Sprintf("cases to sample (at most %d)", hardMaxCases))
	flags.Float64Var(&options.BudgetUSD, "budget-usd", 0, "stop reviewing once this much has been spent")
	flags.StringVar(&options.Seed, "seed", "reviewer-eval-v1", "deterministic sampling seed")
	flags.StringVar(&options.WorkDir, "work-dir", "", "parent for isolated replay repositories (default system temp)")
	reviewerCmd := flags.String("reviewer-cmd", "", "reviewer adapter command (JSON request on stdin, JSON result on stdout)")
	allowReal := flags.Bool("allow-real-reviewer", false, "explicitly allow running the reviewer adapter, which may spend money")
	judgeCmd := flags.String("judge-cmd", "", "optional semantic judge adapter command")
	reportPath := flags.String("report", "", "write the JSON report here (default stdout)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *datasetPath == "" || *reviewerCmd == "" || !*allowReal {
		_, _ = fmt.Fprintln(stderr, "reviewereval replay: -dataset, -reviewer-cmd, and -allow-real-reviewer are required; a real review is always opt-in")
		return 2
	}
	set, err := loadDataset(*datasetPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reviewereval replay: %v\n", err)
		return 1
	}
	match := matcher{}
	if *judgeCmd != "" {
		match.judge = commandJudge{argv: strings.Fields(*judgeCmd)}
	}
	report, err := replay(ctx, set, gitReader{dir: *repo}, commandReviewer{argv: strings.Fields(*reviewerCmd)}, match, options)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reviewereval replay: %v\n", err)
		return 2
	}
	printSummary(stderr, report)
	if err := writeOutput(*reportPath, stdout, func(w io.Writer) error { return encodeJSON(w, report) }); err != nil {
		_, _ = fmt.Fprintf(stderr, "reviewereval replay: %v\n", err)
		return 1
	}
	return 0
}

func runCompare(_ context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("compare", flag.ContinueOnError)
	flags.SetOutput(stderr)
	baselinePath := flags.String("baseline", "", "baseline variant's replay report")
	candidatePath := flags.String("candidate", "", "candidate variant's replay report")
	publishRoot := flags.String("publish", "", "publish the candidate summary with its comparison to this Goobers instance root")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *baselinePath == "" || *candidatePath == "" {
		_, _ = fmt.Fprintln(stderr, "reviewereval compare: -baseline and -candidate are required")
		return 2
	}
	baseline, errBaseline := loadReport(*baselinePath)
	candidate, errCandidate := loadReport(*candidatePath)
	if err := errors.Join(errBaseline, errCandidate); err != nil {
		_, _ = fmt.Fprintf(stderr, "reviewereval compare: %v\n", err)
		return 1
	}
	comparison, err := compare(baseline, candidate)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reviewereval compare: %v\n", err)
		return 1
	}
	summary := candidate.summary()
	summary.Comparison = &comparison
	if err := encodeJSON(stdout, summary); err != nil {
		_, _ = fmt.Fprintf(stderr, "reviewereval compare: %v\n", err)
		return 1
	}
	if *publishRoot != "" {
		if err := readservice.PublishReviewerEval(*publishRoot, summary); err != nil {
			_, _ = fmt.Fprintf(stderr, "reviewereval compare: %v\n", err)
			return 1
		}
	}
	for _, regression := range comparison.Regressions {
		_, _ = fmt.Fprintf(stderr, "reviewereval: regression: %s\n", regression)
	}
	if !comparison.Promotable {
		return 1
	}
	return 0
}

func runPublish(_ context.Context, args []string, _, stderr io.Writer) int {
	flags := flag.NewFlagSet("publish", flag.ContinueOnError)
	flags.SetOutput(stderr)
	reportPath := flags.String("report", "", "replay report to publish")
	root := flags.String("instance", "", "Goobers instance root")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *reportPath == "" || *root == "" {
		_, _ = fmt.Fprintln(stderr, "reviewereval publish: -report and -instance are required")
		return 2
	}
	report, err := loadReport(*reportPath)
	if err == nil {
		err = readservice.PublishReviewerEval(*root, report.summary())
	}
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "reviewereval publish: %v\n", err)
		return 1
	}
	return 0
}

func loadReport(path string) (evalReport, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return evalReport{}, err
	}
	var report evalReport
	if err := json.Unmarshal(data, &report); err != nil {
		return evalReport{}, fmt.Errorf("decode reviewer eval report %s: %w", path, err)
	}
	if report.Schema != reportSchemaVersion {
		return evalReport{}, fmt.Errorf("unsupported reviewer eval report schema %q in %s", report.Schema, path)
	}
	return report, nil
}

func printSummary(out io.Writer, report evalReport) {
	_, _ = fmt.Fprintf(out, "reviewer eval %s %s/%s: %d sampled, %d reviewed, %d failed, %d over budget, %d out-of-diff\n",
		report.DatasetVersion, report.Split, report.Variant, report.Sampled, report.Reviewed, report.Failed,
		report.BudgetExhausted, report.OutOfDiff)
	_, _ = fmt.Fprintf(out, "  overall recall %d/%d = %.4f precision %d/%d = %.4f cost/bug $%.4f\n",
		report.Overall.Caught, report.Overall.Evaluated, report.Overall.Recall,
		report.Overall.MatchedFindings, report.Overall.Findings, report.Overall.Precision, report.CostPerBugUSD)
	for _, class := range report.Classes {
		_, _ = fmt.Fprintf(out, "  %-16s recall %.4f precision %d/%d = %.4f\n",
			class.Class, class.Recall, class.MatchedFindings, class.Findings, class.Precision)
	}
}

func writeOutput(path string, stdout io.Writer, write func(io.Writer) error) error {
	if path == "" {
		return write(stdout)
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := write(file); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func encodeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
