package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/goobers/goobers/api/validate"
	buildversion "github.com/goobers/goobers/internal/version"
)

const (
	diagnosticsSchemaVersion = "goobers.dev/diagnostics/v1"
	featuresSchemaVersion    = "goobers.dev/features/v1"
	diagnosticSeverityInfo   = "info"
)

type diagnosticFinding struct {
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	Col      int    `json:"col,omitempty"`
	Path     string `json:"path,omitempty"`
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type diagnosticCounts struct {
	Errors   int `json:"errors"`
	Warnings int `json:"warnings"`
	Infos    int `json:"infos,omitempty"`
}

type diagnosticsEnvelope struct {
	SchemaVersion string              `json:"schemaVersion"`
	Version       string              `json:"version"`
	Commit        string              `json:"commit"`
	OK            bool                `json:"ok"`
	Counts        diagnosticCounts    `json:"counts"`
	Findings      []diagnosticFinding `json:"findings"`
}

type diagnosticCollector struct {
	findings []diagnosticFinding
}

func (c *diagnosticCollector) add(file, path, code, severity, message string) {
	c.addLocated(file, path, code, severity, message, 0, 0)
}

func (c *diagnosticCollector) addLocated(file, path, code, severity, message string, line, col int) {
	if c == nil {
		return
	}
	if file == "" {
		file = "."
	}
	if path == "" {
		path = diagnosticPath(message)
	}
	finding := diagnosticFinding{
		File:     filepath.ToSlash(file),
		Path:     path,
		Code:     code,
		Severity: severity,
		Message:  message,
	}
	finding.Line, finding.Col = line, col
	if finding.Line == 0 {
		finding.Line, finding.Col = diagnosticLineCol(message)
	}
	if finding.Line > 0 && finding.Col == 0 {
		finding.Col = 1
	}
	c.findings = append(c.findings, finding)
}

func (c *diagnosticCollector) addReport(report *validate.Report, configBase string) {
	if c == nil || report == nil {
		return
	}
	for _, issue := range report.Issues {
		file := configBase
		if issue.File != "" {
			file = joinDiagnosticPath(configBase, issue.File)
		}
		c.addLocated(file, "", string(issue.Code), string(issue.Severity), issue.Message, issue.Line, issue.Col)
	}
}

func (c *diagnosticCollector) envelope(ok bool) diagnosticsEnvelope {
	info := buildversion.Get()
	findings := c.findings
	if findings == nil {
		findings = []diagnosticFinding{}
	}
	envelope := diagnosticsEnvelope{
		SchemaVersion: diagnosticsSchemaVersion,
		Version:       info.Version,
		Commit:        info.Commit,
		OK:            ok,
		Findings:      findings,
	}
	for _, finding := range findings {
		switch finding.Severity {
		case string(validate.Error):
			envelope.Counts.Errors++
		case string(validate.Warning):
			envelope.Counts.Warnings++
		case diagnosticSeverityInfo:
			envelope.Counts.Infos++
		}
	}
	return envelope
}

// emitGitHubAnnotations renders each collected finding as a GitHub Actions
// workflow command (error/warning, https://docs.github.com/actions/using-
// workflows/workflow-commands-for-github-actions#setting-an-error-message),
// anchored to its file/line/col when known. This is #687's config-repo PR
// gate: it turns a validate/lint run's findings into annotations that land
// directly on the PR diff, instead of only a raw log a reviewer has to open.
// Written to stderr, not stdout, deliberately: composing --github-annotations
// with --json must not interleave workflow-command lines into the JSON
// envelope written to stdout.
func emitGitHubAnnotations(w io.Writer, diagnostics *diagnosticCollector) {
	if diagnostics == nil {
		return
	}
	for _, finding := range diagnostics.findings {
		command := "notice"
		switch finding.Severity {
		case string(validate.Error):
			command = "error"
		case string(validate.Warning):
			command = "warning"
		}
		properties := "file=" + githubAnnotationEscapeProperty(finding.File)
		if finding.Line > 0 {
			properties += fmt.Sprintf(",line=%d", finding.Line)
			if finding.Col > 0 {
				properties += fmt.Sprintf(",col=%d", finding.Col)
			}
		}
		title := finding.Code
		if title == "" {
			title = "goobers-validate"
		}
		properties += ",title=" + githubAnnotationEscapeProperty(title)
		pf(w, "::%s %s::%s\n", command, properties, githubAnnotationEscapeData(finding.Message))
	}
}

// githubAnnotationEscapeData escapes a workflow command's message body per
// GitHub's documented rule: percent, CR, and LF.
func githubAnnotationEscapeData(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	s = strings.ReplaceAll(s, "\r", "%0D")
	s = strings.ReplaceAll(s, "\n", "%0A")
	return s
}

// githubAnnotationEscapeProperty escapes a workflow command property value
// per GitHub's documented rule: the data escapes plus colon and comma, since
// those delimit the property list itself.
func githubAnnotationEscapeProperty(s string) string {
	s = githubAnnotationEscapeData(s)
	s = strings.ReplaceAll(s, ":", "%3A")
	s = strings.ReplaceAll(s, ",", "%2C")
	return s
}

func encodeSchemaJSON(w io.Writer, schemaFile string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := validateSchemaJSON(schemaFile, data); err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func diagnosticFile(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(path)
	}
	if rel == "." {
		return "."
	}
	return filepath.ToSlash(rel)
}

func joinDiagnosticPath(base, path string) string {
	if base == "." || base == "" {
		return filepath.ToSlash(path)
	}
	return filepath.ToSlash(filepath.Join(base, filepath.FromSlash(path)))
}

var (
	documentPathPattern = regexp.MustCompile(`\b(?:apiVersion|kind|dslVersion|metadata|spec)(?:\.[A-Za-z][A-Za-z0-9_-]*|\[[0-9]+\])*`)
	linePattern         = regexp.MustCompile(`(?i)\bline[ :]+([0-9]+)(?:[,: ]+column[ :]+([0-9]+))?`)
)

func diagnosticPath(message string) string {
	if strings.HasPrefix(message, "/") {
		if end := strings.Index(message, ":"); end > 0 {
			return message[:end]
		}
	}
	field := documentPathPattern.FindString(message)
	if field == "" {
		switch {
		case strings.HasPrefix(message, "start state "):
			return "/spec/start"
		case strings.HasPrefix(message, "task "):
			return "/spec/tasks"
		case strings.HasPrefix(message, "gate "):
			return "/spec/gates"
		case strings.Contains(message, "has no schedule trigger"):
			return "/spec/triggers"
		default:
			return "/"
		}
	}
	field = strings.ReplaceAll(field, ".", "/")
	field = strings.ReplaceAll(field, "[", "/")
	field = strings.ReplaceAll(field, "]", "")
	return "/" + field
}

func diagnosticLineCol(message string) (int, int) {
	match := linePattern.FindStringSubmatch(message)
	if len(match) == 0 {
		return 0, 0
	}
	line, _ := strconv.Atoi(match[1])
	col := 0
	if len(match) > 2 && match[2] != "" {
		col, _ = strconv.Atoi(match[2])
	}
	return line, col
}
