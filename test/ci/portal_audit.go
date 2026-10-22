package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const portalAuditAttempts = 3

var portalAuditHTTP5xx = regexp.MustCompile(`\b5[0-9]{2}\b`)

type portalAuditOutcome int

const (
	portalAuditPassed portalAuditOutcome = iota
	portalAuditFindings
	portalAuditUnavailable
	portalAuditFailed
)

type auditAttempt func(string) ([]byte, error)

func runPortalAudit(npm string, stdout, stderr io.Writer, attempt auditAttempt, sleep func(time.Duration)) int {
	for number := 1; number <= portalAuditAttempts; number++ {
		output, err := attempt(npm)
		outcome := classifyPortalAudit(output, err)
		switch outcome {
		case portalAuditPassed:
			return 0
		case portalAuditFindings, portalAuditFailed:
			_, _ = stdout.Write(output)
			if len(output) > 0 && output[len(output)-1] != '\n' {
				_, _ = fmt.Fprintln(stdout)
			}
			if err != nil {
				_, _ = fmt.Fprintf(stderr, "portal-audit: %v\n", err)
			}
			return 1
		case portalAuditUnavailable:
			if number < portalAuditAttempts {
				sleep(time.Duration(number*number) * time.Second)
				continue
			}
			message := auditUnavailableMessage(output)
			_, _ = fmt.Fprintf(stderr,
				"::warning title=Portal dependency audit unavailable::npm advisory service remained unavailable after %d bounded attempts; lockfile-pinned dependencies were not audited (%s)\n",
				portalAuditAttempts, message)
			return 0
		}
	}
	return 1
}

func runNPMAudit(npm string) ([]byte, error) {
	return exec.Command(npm, "--prefix", "portal", "audit", "--audit-level=low", "--json").CombinedOutput()
}

func classifyPortalAudit(output []byte, err error) portalAuditOutcome {
	if err == nil {
		return portalAuditPassed
	}
	if auditHasFindings(output) {
		return portalAuditFindings
	}
	if auditServiceUnavailable(output) {
		return portalAuditUnavailable
	}
	return portalAuditFailed
}

func auditHasFindings(output []byte) bool {
	var report struct {
		Metadata struct {
			Vulnerabilities map[string]int `json:"vulnerabilities"`
		} `json:"metadata"`
	}
	if json.Unmarshal(output, &report) != nil {
		return false
	}
	for _, count := range report.Metadata.Vulnerabilities {
		if count > 0 {
			return true
		}
	}
	return false
}

func auditServiceUnavailable(output []byte) bool {
	text := strings.ToLower(string(output))
	for _, marker := range []string{
		"e503", "service unavailable", "eai_again", "econnreset",
		"econnrefused", "etimedout", "enetunreach", "socket hang up",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return portalAuditHTTP5xx.Match(output)
}

func auditUnavailableMessage(output []byte) string {
	var report struct {
		Error struct {
			Code    string `json:"code"`
			Summary string `json:"summary"`
		} `json:"error"`
	}
	if json.Unmarshal(output, &report) == nil {
		if message := strings.TrimSpace(strings.Join([]string{report.Error.Code, report.Error.Summary}, " ")); message != "" {
			return message
		}
	}
	message := strings.TrimSpace(string(output))
	if message == "" {
		return "network or 5xx response"
	}
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 240 {
		message = message[:240] + "..."
	}
	return message
}
