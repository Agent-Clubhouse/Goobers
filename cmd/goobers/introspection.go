package main

import (
	"encoding/json"
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
	finding.Line, finding.Col = diagnosticLineCol(message)
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
		c.add(file, "", diagnosticCode(issue), string(issue.Severity), issue.Message)
	}
}

func diagnosticCode(issue validate.Issue) string {
	if issue.Code != "" {
		return string(issue.Code)
	}
	switch {
	case strings.HasPrefix(issue.Message, "invalid YAML:"):
		return "YAML001"
	case strings.HasPrefix(issue.Message, "/"):
		return "SCHEMA001"
	case strings.Contains(issue.Message, "apiVersion/kind"):
		return "SCHEMA002"
	case strings.HasPrefix(issue.Message, "unknown kind "):
		return "SCHEMA003"
	case strings.Contains(issue.Message, "capability"):
		return "CAP001"
	case strings.HasPrefix(issue.Message, "spec.docsRoots"):
		return "DOCS001"
	case strings.HasPrefix(issue.Message, "start state "):
		return "WF001"
	case strings.HasPrefix(issue.Message, "task "), strings.HasPrefix(issue.Message, "gate "):
		return "WF002"
	case strings.Contains(issue.Message, "references "), strings.Contains(issue.Message, " is not defined"),
		strings.Contains(issue.Message, "names "):
		return "REF001"
	case strings.HasPrefix(issue.Message, "duplicate "):
		return "CFG002"
	default:
		return "CFG001"
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
		}
	}
	return envelope
}

func encodeIndentedJSON(w io.Writer, value any) error {
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
