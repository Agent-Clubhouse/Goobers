package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	datasetSchemaVersion = "goobers.dev/backprop/reviewer-eval-dataset/v1"

	splitTrain = "train"
	splitEval  = "eval"

	// splitRule documents how a case's split is derived; it is recorded in
	// every dataset header so a reader can recompute it.
	splitRule = "train when the first byte of sha256(introducingCommit) mod 5 is 0, else eval"
)

// R-SZZ filters: each removes fix lines (or, for pure-refactor, blamed
// commits) that must not count as the bug's culprit.
const (
	// filterWhitespace drops blank or whitespace-only deleted lines.
	filterWhitespace = "whitespace"
	// filterFormat drops deleted lines the same fix hunk re-adds with only
	// whitespace changed (reformatting).
	filterFormat = "format"
	// filterMove drops deleted lines the fix re-adds elsewhere (moved code).
	filterMove = "move"
	// filterPureRefactor re-blames past a commit whose every change is a
	// whitespace, format, or move change, so it never counts as the culprit.
	filterPureRefactor = "pure-refactor"
)

// Reasons a traced fix yields no case.
const (
	dropNoTraceableLines = "no-traceable-lines"
	dropAllLinesFiltered = "all-lines-filtered"
	dropAmbiguous        = "ambiguous"
	dropRootCommit       = "root-commit"
)

// Flags on a kept case.
const (
	flagMultipleCandidates = "multiple-candidates"
	flagOutOfDiff          = "out-of-diff"
)

// datasetHeader is the first JSONL line of a dataset: what range was traced,
// how cases were split, and how much each R-SZZ filter removed.
type datasetHeader struct {
	Schema     string         `json:"schema"`
	Version    string         `json:"version"`
	Range      string         `json:"range"`
	FromCommit string         `json:"fromCommit,omitempty"`
	ToCommit   string         `json:"toCommit"`
	SplitRule  string         `json:"splitRule"`
	MaxFixes   int            `json:"maxFixes"`
	Truncated  bool           `json:"truncated,omitempty"`
	FixesFound int            `json:"fixesFound"`
	Filtered   map[string]int `json:"filtered"`
	Dropped    map[string]int `json:"dropped"`
	CaseCount  int            `json:"caseCount"`
	Digest     string         `json:"digest"`
}

// datasetCase is one bug fix traced to the commit that introduced it.
// BugLines are the lines of the introducing commit's version that the fix
// later deleted or modified: where a review of that commit should have
// pointed.
type datasetCase struct {
	ID                string         `json:"id"`
	Split             string         `json:"split"`
	FixCommit         string         `json:"fixCommit"`
	FixSubject        string         `json:"fixSubject"`
	FixSource         string         `json:"fixSource"`
	FixIssues         []int          `json:"fixIssues,omitempty"`
	IntroducingCommit string         `json:"introducingCommit"`
	Confidence        float64        `json:"confidence"`
	Candidates        []candidate    `json:"candidates"`
	Flags             []string       `json:"flags,omitempty"`
	OutOfDiff         bool           `json:"outOfDiff"`
	BugLines          []fileLines    `json:"bugLines"`
	FixFiles          []string       `json:"fixFiles"`
	Filtered          map[string]int `json:"filtered,omitempty"`
	HandLabel         *handLabel     `json:"handLabel,omitempty"`
}

// candidate is one commit blame traced fix lines to, with its share of them.
type candidate struct {
	Commit     string  `json:"commit"`
	Lines      int     `json:"lines"`
	Confidence float64 `json:"confidence"`
}

type fileLines struct {
	Path  string `json:"path"`
	Lines []int  `json:"lines"`
}

// handLabel is a human's verdict on which commit really introduced a fix's
// bug. A small labeled sample calibrates how often R-SZZ blames the right
// commit.
type handLabel struct {
	FixCommit         string `json:"fixCommit,omitempty"`
	IntroducingCommit string `json:"introducingCommit"`
	LabeledBy         string `json:"labeledBy"`
	LabeledAt         string `json:"labeledAt"`
	Note              string `json:"note,omitempty"`
}

type dataset struct {
	Header datasetHeader
	Cases  []datasetCase
}

// splitOf assigns a case's split from its introducing commit, so two fixes
// blaming the same offending change always land in the same split.
func splitOf(introducingCommit string) string {
	sum := sha256.Sum256([]byte(introducingCommit))
	if sum[0]%5 == 0 {
		return splitTrain
	}
	return splitEval
}

// digest is an order-independent content digest of the cases.
func casesDigest(cases []datasetCase) string {
	sorted := append([]datasetCase(nil), cases...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	hash := sha256.New()
	for _, item := range sorted {
		data, _ := json.Marshal(item)
		hash.Write(data)
		hash.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func writeDataset(out io.Writer, set dataset) error {
	set.Header.CaseCount = len(set.Cases)
	set.Header.Digest = casesDigest(set.Cases)
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(set.Header); err != nil {
		return err
	}
	for _, item := range set.Cases {
		if err := encoder.Encode(item); err != nil {
			return err
		}
	}
	return nil
}

func loadDataset(path string) (dataset, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return dataset{}, err
	}
	return parseDataset(data)
}

func parseDataset(data []byte) (dataset, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	var set dataset
	line := 0
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		line++
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.DisallowUnknownFields()
		if line == 1 {
			if err := decoder.Decode(&set.Header); err != nil {
				return dataset{}, fmt.Errorf("decode reviewer eval dataset header: %w", err)
			}
			continue
		}
		var item datasetCase
		if err := decoder.Decode(&item); err != nil {
			return dataset{}, fmt.Errorf("decode reviewer eval dataset line %d: %w", line, err)
		}
		set.Cases = append(set.Cases, item)
	}
	if err := scanner.Err(); err != nil {
		return dataset{}, err
	}
	return set, set.validate()
}

// validate rejects a dataset whose schema, digest, or cases are malformed,
// and one that mixes training and evaluation: every case's split must follow
// the split rule, and no fix or introducing commit may appear in both splits.
func (set dataset) validate() error {
	header := set.Header
	if header.Schema != datasetSchemaVersion {
		return fmt.Errorf("unsupported reviewer eval dataset schema %q", header.Schema)
	}
	if strings.TrimSpace(header.Version) == "" {
		return errors.New("reviewer eval dataset version is required")
	}
	if header.CaseCount != len(set.Cases) {
		return fmt.Errorf("reviewer eval dataset header records %d cases, file holds %d", header.CaseCount, len(set.Cases))
	}
	if digest := casesDigest(set.Cases); header.Digest != digest {
		return fmt.Errorf("reviewer eval dataset digest %s does not match its cases (%s); rebuild instead of editing", header.Digest, digest)
	}
	ids := map[string]bool{}
	splits := map[string]string{}
	for _, item := range set.Cases {
		if err := item.validate(); err != nil {
			return fmt.Errorf("reviewer eval case %q: %w", item.ID, err)
		}
		if ids[item.ID] {
			return fmt.Errorf("duplicate reviewer eval case %q", item.ID)
		}
		ids[item.ID] = true
		for _, key := range []string{"fix:" + item.FixCommit, "introducing:" + item.IntroducingCommit} {
			if split, ok := splits[key]; ok && (split != item.Split || strings.HasPrefix(key, "fix:")) {
				return fmt.Errorf("%s commit appears in more than one case or in both the %s and %s splits", key, split, item.Split)
			}
			splits[key] = item.Split
		}
	}
	return nil
}

func (item datasetCase) validate() error {
	if strings.TrimSpace(item.ID) == "" || item.FixCommit == "" || item.IntroducingCommit == "" {
		return errors.New("case ID, fix commit, and introducing commit are required")
	}
	if item.Split != splitOf(item.IntroducingCommit) {
		return fmt.Errorf("split %q does not follow the split rule", item.Split)
	}
	if item.Confidence <= 0 || item.Confidence > 1 || len(item.Candidates) == 0 || item.Candidates[0].Commit != item.IntroducingCommit {
		return errors.New("introducing commit must be the most confident candidate, with confidence in (0,1]")
	}
	if len(item.BugLines) == 0 {
		return errors.New("a case needs the bug lines the fix touched")
	}
	if item.HandLabel != nil {
		if strings.TrimSpace(item.HandLabel.IntroducingCommit) == "" || strings.TrimSpace(item.HandLabel.LabeledBy) == "" {
			return errors.New("a hand label needs its introducing commit and labeler")
		}
		if _, err := time.Parse(time.DateOnly, item.HandLabel.LabeledAt); err != nil {
			return fmt.Errorf("hand label labeledAt must be a date: %w", err)
		}
	}
	return nil
}
