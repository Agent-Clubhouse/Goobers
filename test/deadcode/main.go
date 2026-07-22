// Command deadcode reports unreachable production functions that are not in
// the repository's reviewed exemption list.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

const defaultExemptionsPath = "test/deadcode/exemptions.txt"

type reportPackage struct {
	Path  string           `json:"Path"`
	Funcs []reportFunction `json:"Funcs"`
}

type reportFunction struct {
	Name     string         `json:"Name"`
	Position reportPosition `json:"Position"`
}

type reportPosition struct {
	File string `json:"File"`
	Line int    `json:"Line"`
	Col  int    `json:"Col"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("deadcode", flag.ContinueOnError)
	flags.SetOutput(stderr)
	goCommand := flags.String("go", "go", "Go command to use")
	exemptionsPath := flags.String("exemptions", defaultExemptionsPath, "reviewed exemption list")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	patterns := flags.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	exemptionsFile, err := os.Open(*exemptionsPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "deadcode: read exemptions: %v\n", err)
		return 1
	}
	exemptions, err := parseExemptions(exemptionsFile)
	closeErr := exemptionsFile.Close()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "deadcode: read exemptions: %v\n", err)
		return 1
	}
	if closeErr != nil {
		_, _ = fmt.Fprintf(stderr, "deadcode: close exemptions: %v\n", closeErr)
		return 1
	}

	reports, err := analyze(*goCommand, patterns, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "deadcode: analyze: %v\n", err)
		return 1
	}
	if problems := exemptionProblems(reports, exemptions); len(problems) > 0 {
		for _, problem := range problems {
			_, _ = fmt.Fprintln(stderr, problem)
		}
		return 1
	}

	_, _ = fmt.Fprintln(stdout, "deadcode: no unreviewed unreachable functions")
	return 0
}

func analyze(goCommand string, patterns []string, stderr io.Writer) ([]reportPackage, error) {
	args := analyzerArgs(patterns)
	command := exec.Command(goCommand, args...)
	command.Stderr = stderr
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	return decodeReports(output)
}

func analyzerArgs(patterns []string) []string {
	args := []string{"tool", "deadcode", "-json"}
	return append(args, patterns...)
}

func parseExemptions(r io.Reader) (map[string]string, error) {
	exemptions := make(map[string]string)
	scanner := bufio.NewScanner(r)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		symbol, reason, ok := strings.Cut(line, " # ")
		symbol = strings.TrimSpace(symbol)
		reason = strings.TrimSpace(reason)
		if !ok || symbol == "" || reason == "" {
			return nil, fmt.Errorf("line %d: want <exact package.symbol> # <reason>", lineNumber)
		}
		if strings.ContainsAny(symbol, " \t") {
			return nil, fmt.Errorf("line %d: symbol %q contains whitespace", lineNumber, symbol)
		}
		if _, duplicate := exemptions[symbol]; duplicate {
			return nil, fmt.Errorf("line %d: duplicate symbol %q", lineNumber, symbol)
		}
		exemptions[symbol] = reason
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return exemptions, nil
}

func exemptionProblems(reports []reportPackage, exemptions map[string]string) []string {
	seen := make(map[string]bool)
	var problems []string
	for _, report := range reports {
		for _, function := range report.Funcs {
			symbol := report.Path + "." + function.Name
			seen[symbol] = true
			if _, exempt := exemptions[symbol]; exempt {
				continue
			}
			position := function.Position
			problems = append(problems, fmt.Sprintf(
				"%s:%d:%d: unreviewed dead code: %s",
				position.File, position.Line, position.Col, symbol,
			))
		}
	}
	for symbol, reason := range exemptions {
		if !seen[symbol] {
			problems = append(problems, fmt.Sprintf(
				"stale deadcode exemption: %s (%s)",
				symbol, reason,
			))
		}
	}
	sort.Strings(problems)
	return problems
}

func decodeReports(data []byte) ([]reportPackage, error) {
	var reports []reportPackage
	if err := json.Unmarshal(data, &reports); err != nil {
		return nil, fmt.Errorf("decode analyzer output: %w", err)
	}
	return reports, nil
}
