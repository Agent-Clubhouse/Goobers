// Command coveragegate enforces the repository's testable-logic coverage threshold.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultThreshold = "70"
	defaultProfile   = "coverage.out"
	defaultExclude   = `/cmd/|zz_generated|\.deepcopy\.go|/api/schemas/`
)

var profileEntryPattern = regexp.MustCompile(`^(.+):\d+\.\d+,\d+\.\d+\s+\d+\s+\d+$`)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	thresholdText := envOrDefault("COVERAGE_THRESHOLD", defaultThreshold)
	if len(args) > 0 && args[0] != "" {
		thresholdText = args[0]
	}
	threshold, err := parsePercentage(thresholdText)
	if err != nil {
		pf(stderr, "coverage_gate: invalid threshold %q: %v\n", thresholdText, err)
		return 2
	}

	profile := envOrDefault("COVERAGE_PROFILE", defaultProfile)
	excludeText := envOrDefault("COVERAGE_EXCLUDE", defaultExclude)
	exclude, err := regexp.Compile(excludeText)
	if err != nil {
		pf(stderr, "coverage_gate: invalid exclusion regex %q: %v\n", excludeText, err)
		return 2
	}

	if _, err := os.Stat(profile); errors.Is(err, os.ErrNotExist) {
		pf(stdout, "coverage_gate: %s not found - running tests to generate it...\n", profile)
		if err := generateProfile(profile, stdout, stderr); err != nil {
			pf(stderr, "coverage_gate: generate profile: %v\n", err)
			return 2
		}
	} else if err != nil {
		pf(stderr, "coverage_gate: inspect %s: %v\n", profile, err)
		return 2
	}

	raw, err := os.ReadFile(profile)
	if err != nil {
		pf(stderr, "coverage_gate: read %s: %v\n", profile, err)
		return 2
	}
	filtered, excluded, err := filterProfile(raw, exclude)
	if err != nil {
		pf(stderr, "coverage_gate: parse %s: %v\n", profile, err)
		return 2
	}

	filteredFile, err := os.CreateTemp("", "coverage-gate-*.out")
	if err != nil {
		pf(stderr, "coverage_gate: create filtered profile: %v\n", err)
		return 2
	}
	filteredPath := filteredFile.Name()
	defer func() { _ = os.Remove(filteredPath) }()
	if _, err := filteredFile.Write(filtered); err != nil {
		_ = filteredFile.Close()
		pf(stderr, "coverage_gate: write filtered profile: %v\n", err)
		return 2
	}
	if err := filteredFile.Close(); err != nil {
		pf(stderr, "coverage_gate: close filtered profile: %v\n", err)
		return 2
	}

	pf(stdout, "=== excluded from coverage denominator (regex: %s) ===\n", excludeText)
	if len(excluded) == 0 {
		pln(stdout, "  (nothing matched the exclusion regex)")
	} else {
		for _, file := range excluded {
			pf(stdout, "  %s\n", file)
		}
	}

	pln(stdout, "")
	pln(stdout, "=== coverage by function (after exclusions) ===")
	report, err := functionCoverage(filteredPath)
	if err != nil {
		pf(stderr, "coverage_gate: calculate coverage: %v\n", err)
		return 2
	}
	_, _ = stdout.Write(report)

	totalText, total, err := parseTotalCoverage(report)
	if err != nil {
		pf(stderr, "coverage_gate: %v\n", err)
		return 2
	}

	pln(stdout, "")
	pf(stdout, "testable-logic coverage: %s%%  (threshold: %s%%)\n", totalText, thresholdText)
	if belowThreshold(total, threshold) {
		pf(stderr, "FAIL: coverage %s%% is below threshold %s%%\n", totalText, thresholdText)
		return 1
	}
	pln(stdout, "PASS: coverage gate satisfied")
	return 0
}

func filterProfile(raw []byte, exclude *regexp.Regexp) ([]byte, []string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, nil, err
		}
		return nil, nil, errors.New("profile is empty")
	}
	mode := scanner.Text()
	switch mode {
	case "mode: set", "mode: count", "mode: atomic":
	default:
		return nil, nil, fmt.Errorf("invalid mode line %q", mode)
	}

	var filtered bytes.Buffer
	filtered.WriteString(mode)
	filtered.WriteByte('\n')
	excludedSet := make(map[string]struct{})
	lineNumber := 1
	for scanner.Scan() {
		lineNumber++
		line := scanner.Text()
		match := profileEntryPattern.FindStringSubmatch(line)
		if match == nil {
			return nil, nil, fmt.Errorf("invalid profile entry on line %d: %q", lineNumber, line)
		}
		if exclude.MatchString(line) {
			excludedSet[match[1]] = struct{}{}
			continue
		}
		filtered.WriteString(line)
		filtered.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, err
	}

	excluded := make([]string, 0, len(excludedSet))
	for file := range excludedSet {
		excluded = append(excluded, file)
	}
	sort.Strings(excluded)
	return filtered.Bytes(), excluded, nil
}

func parseTotalCoverage(report []byte) (string, float64, error) {
	scanner := bufio.NewScanner(bytes.NewReader(report))
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "total:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			break
		}
		totalText := strings.TrimSuffix(fields[len(fields)-1], "%")
		total, err := parsePercentage(totalText)
		if err != nil {
			return "", 0, fmt.Errorf("could not parse total coverage from %q: %w", line, err)
		}
		return totalText, total, nil
	}
	if err := scanner.Err(); err != nil {
		return "", 0, err
	}
	return "", 0, errors.New("could not find total coverage")
}

func parsePercentage(value string) (float64, error) {
	percentage, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(percentage) || math.IsInf(percentage, 0) {
		return 0, errors.New("percentage must be finite")
	}
	return percentage, nil
}

func belowThreshold(total, threshold float64) bool {
	return total < threshold
}

func generateProfile(profile string, stdout, stderr io.Writer) error {
	cmd := exec.Command("go", "test", "./...", "-covermode=atomic", "-coverprofile="+profile)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

func functionCoverage(profile string) ([]byte, error) {
	cmd := exec.Command("go", "tool", "cover", "-func="+profile)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func pf(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func pln(w io.Writer, line string) {
	_, _ = fmt.Fprintln(w, line)
}
