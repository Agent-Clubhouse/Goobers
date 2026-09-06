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
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
)

const defaultExemptionsPath = "test/deadcode/exemptions.txt"

// deadcodeToolPackage is the module-tool import path `go.mod`'s `tool`
// directive names. Built once, natively, per run — see buildAnalyzer for why
// this cannot be `go tool deadcode` invoked with GOOS overridden in its own
// environment.
const deadcodeToolPackage = "golang.org/x/tools/cmd/deadcode"

// targetGOOS names every platform this gate evaluates reachability against
// (#4434). golang.org/x/tools/cmd/deadcode's own docs state its analysis is
// valid for exactly one GOOS/GOARCH/-tags configuration, so a symbol reachable
// only via a `//go:build windows` caller (or only via a `//go:build !windows`
// one) is genuinely dead under the OTHER configurations — checking every
// shipped target here is what keeps a Windows-only or Unix-only symbol from
// being reported as dead-everywhere, or its exemption from being reported
// stale-everywhere, on the strength of whichever single OS happened to run
// the check.
var targetGOOS = []string{"linux", "darwin", "windows"}

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

// exemption is one reviewed entry. Platforms is nil for an unqualified entry
// (#4434's backward-compatible default: expected dead on every target
// platform, exactly the pre-existing single-platform behavior this list was
// authored under); a non-nil Platforms is the explicit subset of targetGOOS
// the symbol is expected to be dead on — e.g. a Windows-only caller means the
// symbol is dead on linux/darwin and reachable (not dead, no exemption
// needed) on windows.
type exemption struct {
	platforms []string
	reason    string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("deadcode", flag.ContinueOnError)
	flags.SetOutput(stderr)
	goCommand := flags.String("go", "go", "Go command used to build the analyzer tool and run its per-platform analysis")
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

	analyzerBinary, cleanup, err := buildAnalyzer(*goCommand, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "deadcode: build analyzer: %v\n", err)
		return 1
	}
	defer cleanup()

	platformReports := make(map[string][]reportPackage, len(targetGOOS))
	for _, goos := range targetGOOS {
		reports, err := analyze(analyzerBinary, goos, patterns, stderr)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "deadcode: analyze (GOOS=%s): %v\n", goos, err)
			return 1
		}
		platformReports[goos] = reports
	}
	if problems := exemptionProblems(platformReports, exemptions); len(problems) > 0 {
		for _, problem := range problems {
			_, _ = fmt.Fprintln(stderr, problem)
		}
		return 1
	}

	_, _ = fmt.Fprintln(stdout, "deadcode: no unreviewed unreachable functions")
	return 0
}

// buildAnalyzer builds the deadcode tool ONCE, natively (host GOOS/GOARCH,
// ambient environment), and returns its path plus a cleanup func.
//
// It must be a plain `go build`, never `go tool deadcode` invoked with GOOS
// overridden in ITS OWN environment: `go tool` builds (and caches) the
// requested tool for whatever GOOS/GOARCH its own process environment names,
// then executes that binary in the SAME process — so overriding GOOS to
// cross-compile the target ANALYSIS also cross-compiles the analyzer tool
// itself, which the host then cannot execute at all ("exec format error").
// Building the binary here, under the host's own unmodified environment, and
// only setting GOOS on ITS subprocess env in analyze (which affects the
// `go list`/packages loading the ALREADY-NATIVE binary performs internally
// for the patterns being analyzed) is what actually achieves per-target
// reachability analysis from one host — the same distinction golangci-lint's
// own cross-GOOS invocation in this repo's lint step draws.
func buildAnalyzer(goCommand string, stderr io.Writer) (string, func(), error) {
	dir, err := os.MkdirTemp("", "goobers-deadcode-tool-*")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	binary := filepath.Join(dir, "deadcode")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	command := exec.Command(goCommand, "build", "-o", binary, deadcodeToolPackage)
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		cleanup()
		return "", nil, err
	}
	return binary, cleanup, nil
}

// analyze runs the already-built, host-native analyzerBinary with GOOS
// overridden in its OWN subprocess environment so its internal package
// loading targets goos — see buildAnalyzer for why the binary itself must
// never be built with GOOS overridden.
func analyze(analyzerBinary string, goos string, patterns []string, stderr io.Writer) ([]reportPackage, error) {
	args := analyzerArgs(patterns)
	command := exec.Command(analyzerBinary, args...)
	command.Env = append(os.Environ(), "GOOS="+goos)
	command.Stderr = stderr
	output, err := command.Output()
	if err != nil {
		return nil, err
	}
	return decodeReports(output)
}

func analyzerArgs(patterns []string) []string {
	args := []string{"-json"}
	return append(args, patterns...)
}

// parseExemptions reads `[platform,platform] <exact package.symbol> # <reason>`
// lines, the `[platform,...]` prefix optional. An unqualified line is exempt
// on every entry of targetGOOS.
func parseExemptions(r io.Reader) (map[string]exemption, error) {
	exemptions := make(map[string]exemption)
	scanner := bufio.NewScanner(r)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		platforms, rest, err := cutPlatformPrefix(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNumber, err)
		}
		symbol, reason, ok := strings.Cut(rest, " # ")
		symbol = strings.TrimSpace(symbol)
		reason = strings.TrimSpace(reason)
		if !ok || symbol == "" || reason == "" {
			return nil, fmt.Errorf("line %d: want [<platform,...>] <exact package.symbol> # <reason>", lineNumber)
		}
		if strings.ContainsAny(symbol, " \t") {
			return nil, fmt.Errorf("line %d: symbol %q contains whitespace", lineNumber, symbol)
		}
		if _, duplicate := exemptions[symbol]; duplicate {
			return nil, fmt.Errorf("line %d: duplicate symbol %q", lineNumber, symbol)
		}
		exemptions[symbol] = exemption{platforms: platforms, reason: reason}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return exemptions, nil
}

// cutPlatformPrefix splits an optional leading "[goos,goos] " qualifier off
// line, validating every named platform against targetGOOS. A line with no
// "[" prefix returns a nil platform list (unqualified: all platforms) and the
// line unchanged.
func cutPlatformPrefix(line string) ([]string, string, error) {
	if !strings.HasPrefix(line, "[") {
		return nil, line, nil
	}
	end := strings.Index(line, "]")
	if end < 0 {
		return nil, "", fmt.Errorf("unterminated platform qualifier: %q", line)
	}
	rawPlatforms := strings.Split(line[1:end], ",")
	platforms := make([]string, 0, len(rawPlatforms))
	for _, raw := range rawPlatforms {
		platform := strings.TrimSpace(raw)
		if platform == "" {
			return nil, "", fmt.Errorf("empty platform in qualifier: %q", line)
		}
		if !slices.Contains(targetGOOS, platform) {
			return nil, "", fmt.Errorf("platform %q is not one of %v", platform, targetGOOS)
		}
		platforms = append(platforms, platform)
	}
	if len(platforms) == 0 {
		return nil, "", fmt.Errorf("empty platform qualifier: %q", line)
	}
	return platforms, strings.TrimSpace(line[end+1:]), nil
}

// exemptionProblems evaluates every target platform's report together, so a
// symbol dead only on one platform is judged against exactly the platforms it
// was actually reported dead on, never flagged (or exempted) on the strength
// of a single arbitrary run.
func exemptionProblems(platformReports map[string][]reportPackage, exemptions map[string]exemption) []string {
	deadOn := make(map[string][]string, 64)
	positions := make(map[string]reportPosition, 64)
	for _, goos := range targetGOOS {
		for _, report := range platformReports[goos] {
			if strings.Contains(report.Path, "/test/") {
				continue
			}
			for _, function := range report.Funcs {
				symbol := report.Path + "." + function.Name
				deadOn[symbol] = append(deadOn[symbol], goos)
				if _, recorded := positions[symbol]; !recorded {
					positions[symbol] = function.Position
				}
			}
		}
	}

	var problems []string
	for symbol, deadPlatforms := range deadOn {
		ex, exists := exemptions[symbol]
		uncovered := uncoveredPlatforms(deadPlatforms, ex, exists)
		if len(uncovered) == 0 {
			continue
		}
		position := positions[symbol]
		platformNote := ""
		if len(uncovered) < len(targetGOOS) {
			platformNote = fmt.Sprintf(" [platforms: %s]", strings.Join(uncovered, ","))
		}
		problems = append(problems, fmt.Sprintf(
			"%s:%d:%d: unreviewed dead code: %s%s",
			position.File, position.Line, position.Col, symbol, platformNote,
		))
	}
	for symbol, ex := range exemptions {
		declared := ex.platforms
		if declared == nil {
			declared = targetGOOS
		}
		if !platformsIntersect(declared, deadOn[symbol]) {
			problems = append(problems, fmt.Sprintf(
				"stale deadcode exemption: %s (%s)",
				symbol, ex.reason,
			))
		}
	}
	sort.Strings(problems)
	return problems
}

// uncoveredPlatforms is the subset of deadPlatforms the exemption does not
// account for — empty means every platform the symbol was found dead on is a
// reviewed, expected one. exists is false when symbol has no exemption entry
// at all, in which case every dead platform is uncovered.
func uncoveredPlatforms(deadPlatforms []string, ex exemption, exists bool) []string {
	if !exists {
		uncovered := append([]string(nil), deadPlatforms...)
		sort.Strings(uncovered)
		return uncovered
	}
	if ex.platforms == nil {
		// An unqualified entry is exempt on every target platform.
		return nil
	}
	var uncovered []string
	for _, platform := range deadPlatforms {
		if !slices.Contains(ex.platforms, platform) {
			uncovered = append(uncovered, platform)
		}
	}
	sort.Strings(uncovered)
	return uncovered
}

func platformsIntersect(a, b []string) bool {
	for _, platform := range a {
		if slices.Contains(b, platform) {
			return true
		}
	}
	return false
}

func decodeReports(data []byte) ([]reportPackage, error) {
	var reports []reportPackage
	if err := json.Unmarshal(data, &reports); err != nil {
		return nil, fmt.Errorf("decode analyzer output: %w", err)
	}
	return reports, nil
}
