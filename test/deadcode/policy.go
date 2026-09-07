package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Frozen at 41801d7d8: only byte-equivalent parsed entries can use the rollout
// baseline. Editing a reason/platform or adding a symbol requires current policy.
//
//go:embed policy-baseline.txt
var policyBaseline []byte

const policyBaselineDigest = "d5437d99252d37f3e1c54f81230bceffe6442390ac2c949ed6cd1524dfbf7b76"

type issueStateSnapshot struct {
	Repository string            `json:"repository"`
	ObservedAt string            `json:"observedAt"`
	States     map[string]string `json:"states"`
}

var exemptionIssueRef = regexp.MustCompile(`(?:^|[^[:alnum:]_/])#([1-9][0-9]*)\b`)

func checkExemptionPolicy(entries map[string]exemption, statePath string, now time.Time, details bool, out io.Writer) error {
	digest := sha256.Sum256(policyBaseline)
	if hex.EncodeToString(digest[:]) != policyBaselineDigest {
		return fmt.Errorf("frozen rollout baseline changed; new entries must satisfy policy")
	}
	baseline, err := parseExemptions(bytes.NewReader(policyBaseline))
	if err != nil {
		return err
	}
	snapshot, err := loadIssueStates(statePath)
	if err != nil {
		return err
	}
	observed, _ := time.Parse("2006-01-02", snapshot.ObservedAt)
	if now.Sub(observed) > 30*24*time.Hour {
		_, _ = fmt.Fprintf(out, "deadcode: WARNING: issue-state snapshot is older than 30 days (%s); refresh it before relying on issue state\n", snapshot.ObservedAt)
	}
	var failures, legacy []string
	for symbol, entry := range entries {
		problem := exemptionPolicyProblem(entry.reason, snapshot.States, now)
		if problem == "" {
			continue
		}
		line := symbol + ": " + problem
		if old, ok := baseline[symbol]; ok && reflect.DeepEqual(old, entry) {
			legacy = append(legacy, line)
		} else {
			failures = append(failures, line)
		}
	}
	sort.Strings(legacy)
	if len(legacy) > 0 {
		_, _ = fmt.Fprintf(out, "deadcode: %d existing exemption-policy findings retained in frozen rollout baseline (use -policy-details to list)\n", len(legacy))
		if details {
			for _, line := range legacy {
				_, _ = fmt.Fprintln(out, "deadcode: baseline: "+line)
			}
		}
	}
	if len(failures) > 0 {
		sort.Strings(failures)
		return fmt.Errorf("\n%s", strings.Join(failures, "\n"))
	}
	return nil
}

func loadIssueStates(path string) (issueStateSnapshot, error) {
	var snapshot issueStateSnapshot
	data, err := os.ReadFile(path)
	if err != nil {
		return snapshot, fmt.Errorf("read issue-state evidence: %w", err)
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return snapshot, fmt.Errorf("decode issue-state evidence: %w", err)
	}
	if snapshot.Repository != "Agent-Clubhouse/Goobers" {
		return snapshot, fmt.Errorf("issue-state evidence must name Agent-Clubhouse/Goobers")
	}
	if _, err := time.Parse("2006-01-02", snapshot.ObservedAt); err != nil {
		return snapshot, fmt.Errorf("invalid issue-state observation date: %w", err)
	}
	for number, state := range snapshot.States {
		if state != "open" && state != "closed" {
			return snapshot, fmt.Errorf("unknown state %q for #%s", state, number)
		}
	}
	return snapshot, nil
}

func exemptionPolicyProblem(reason string, states map[string]string, now time.Time) string {
	expires := ""
	for _, field := range strings.Fields(reason) {
		if !strings.HasPrefix(field, "expires=") {
			continue
		}
		if expires != "" {
			return "multiple expires= fields"
		}
		expires = strings.TrimPrefix(field, "expires=")
		if expires == "" {
			return "empty expires= field"
		}
	}
	if expires != "" {
		date, err := time.Parse("2006-01-02", expires)
		if err != nil {
			return "invalid expiry; use expires=YYYY-MM-DD as a separate field"
		}
		// Valid through the named UTC calendar date, never indefinitely.
		if !now.Before(date.AddDate(0, 0, 1)) {
			return "expired on " + expires
		}
		return ""
	}
	refs := exemptionIssueRef.FindAllStringSubmatch(reason, -1)
	unknown := false
	for _, ref := range refs {
		switch states[ref[1]] {
		case "open":
			return ""
		case "closed":
		default:
			unknown = true
		}
	}
	if unknown {
		return "cited issue state is unknown; update issue-states.json or add expires=YYYY-MM-DD"
	}
	if len(refs) > 0 {
		return "cites only closed issues; cite an open owning issue or add expires=YYYY-MM-DD"
	}
	return "requires an open owning issue reference (#NNNN) or expires=YYYY-MM-DD"
}
