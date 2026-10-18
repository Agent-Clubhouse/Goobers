package docchurn

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/configboundary"
	"github.com/goobers/goobers/internal/platform/durability"
)

const (
	schemaVersion          = "goobers.dev/docs-churn/v1"
	watermarkSchemaVersion = "goobers.dev/docs-watermark/v1"
	noChurnNote            = "no code churn in the reported window"
	firstRunNote           = "first run: no watermark yet, bounded to the since-floor window"
	emptyTreeObject        = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
)

// Git runs one Git operation in repo and returns its stdout.
type Git func(repo string, args ...string) (string, error)

// Options contains resolved domain inputs. Clock and Git are required.
type Options struct {
	Repo             string
	WatermarkPath    string
	Gaggle           string
	Workflow         string
	ResultFile       string
	SinceFloor       time.Duration
	BufferMultiplier float64
	DocsRoots        []string
	AdvanceWatermark bool
	Clock            func() time.Time
	Git              Git
	Stdout           io.Writer
}

type docsWatermark struct {
	Schema      string    `json:"schema"`
	Gaggle      string    `json:"gaggle,omitempty"`
	Workflow    string    `json:"workflow"`
	SHA         string    `json:"sha"`
	RefreshedAt time.Time `json:"refreshedAt"`
}

type churnCommit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
	Body    string `json:"body,omitempty"`
}

type docsChurnDigest struct {
	Schema           string              `json:"schema"`
	FirstRun         bool                `json:"firstRun"`
	Since            time.Time           `json:"since"`
	Head             string              `json:"head"`
	Base             string              `json:"base,omitempty"`
	Watermark        *docsWatermark      `json:"watermark,omitempty"`
	BufferMultiplier float64             `json:"bufferMultiplier"`
	SinceFloor       string              `json:"sinceFloor"`
	CommitCount      int                 `json:"commitCount"`
	Commits          []churnCommit       `json:"commits"`
	ChangedFiles     []string            `json:"changedFiles"`
	Areas            map[string][]string `json:"areas"`
	DocsRoots        []string            `json:"docsRoots,omitempty"`
	DocsRootChanges  []string            `json:"docsRootChanges,omitempty"`
	NoWork           bool                `json:"noWork,omitempty"`
	Note             string              `json:"note,omitempty"`
}

// StdoutError identifies an output failure that the CLI reports only through
// its exit status, preserving the command's existing writer-error contract.
type StdoutError struct {
	Err error
}

func (e *StdoutError) Error() string { return e.Err.Error() }
func (e *StdoutError) Unwrap() error { return e.Err }

// Run emits a digest before advancing the watermark.
func Run(opts Options) error {
	watermark, haveWatermark, err := readWatermark(opts.WatermarkPath)
	if err != nil {
		return fmt.Errorf("read docs watermark %s: %w", opts.WatermarkPath, err)
	}

	head, err := revParse(opts.Git, opts.Repo, "HEAD")
	if err != nil {
		return fmt.Errorf("resolve HEAD in %s: %w", opts.Repo, err)
	}

	now := opts.Clock().UTC()
	firstRun := !haveWatermark
	sinceTime := now.Add(-opts.SinceFloor)
	if !firstRun {
		sinceLast := now.Sub(watermark.RefreshedAt)
		if sinceLast < 0 {
			sinceLast = 0
		}
		buffer := time.Duration(float64(sinceLast) * opts.BufferMultiplier)
		if buffer < opts.SinceFloor {
			buffer = opts.SinceFloor
		}
		sinceTime = watermark.RefreshedAt.Add(-buffer)
	}

	base, hasBase, err := boundaryCommit(opts.Git, opts.Repo, sinceTime)
	if err != nil {
		return fmt.Errorf("locate window boundary commit in %s: %w", opts.Repo, err)
	}
	diffBase := base
	if !hasBase {
		diffBase = emptyTreeObject
	}

	commits, err := commitsInRange(opts.Git, opts.Repo, diffBase, head)
	if err != nil {
		return fmt.Errorf("list commits in %s: %w", opts.Repo, err)
	}
	changed, err := changedFiles(opts.Git, opts.Repo, diffBase, head)
	if err != nil {
		return fmt.Errorf("list changed files in %s: %w", opts.Repo, err)
	}

	digest := buildDigest(firstRun, sinceTime, head, base, hasBase, watermark, haveWatermark,
		opts.BufferMultiplier, opts.SinceFloor, commits, changed, opts.DocsRoots)
	if err := writeDigest(digest, opts.ResultFile, opts.Stdout); err != nil {
		return err
	}

	if !opts.AdvanceWatermark {
		return nil
	}
	if err := writeWatermark(opts.WatermarkPath, docsWatermark{
		Schema:      watermarkSchemaVersion,
		Gaggle:      opts.Gaggle,
		Workflow:    opts.Workflow,
		SHA:         head,
		RefreshedAt: now,
	}); err != nil {
		return fmt.Errorf("advance docs watermark %s: %w", opts.WatermarkPath, err)
	}
	return nil
}

func buildDigest(
	firstRun bool,
	sinceTime time.Time,
	head, base string,
	hasBase bool,
	watermark docsWatermark,
	haveWatermark bool,
	multiplier float64,
	floor time.Duration,
	commits []churnCommit,
	changed []string,
	docsRoots []string,
) docsChurnDigest {
	if commits == nil {
		commits = []churnCommit{}
	}
	if changed == nil {
		changed = []string{}
	}
	digest := docsChurnDigest{
		Schema:           schemaVersion,
		FirstRun:         firstRun,
		Since:            sinceTime,
		Head:             head,
		BufferMultiplier: multiplier,
		SinceFloor:       floor.String(),
		CommitCount:      len(commits),
		Commits:          commits,
		ChangedFiles:     changed,
		Areas:            groupByArea(changed),
		DocsRoots:        docsRoots,
		DocsRootChanges:  filesUnderRoots(docsRoots, changed),
	}
	if hasBase {
		digest.Base = base
	}
	if haveWatermark {
		wm := watermark
		digest.Watermark = &wm
	}
	switch {
	case len(changed) == 0 && firstRun:
		digest.NoWork = true
		digest.Note = firstRunNote + "; " + noChurnNote
	case len(changed) == 0:
		digest.NoWork = true
		digest.Note = noChurnNote
	case firstRun:
		digest.Note = firstRunNote
	}
	return digest
}

func writeDigest(digest docsChurnDigest, resultFile string, stdout io.Writer) error {
	out, err := json.MarshalIndent(digest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode churn digest: %w", err)
	}
	out = append(out, '\n')
	if resultFile != "" {
		if err := durability.WriteFileAtomic(resultFile, out, 0o644); err != nil {
			return fmt.Errorf("write result file %q: %w", resultFile, err)
		}
		return nil
	}
	if _, err := stdout.Write(out); err != nil {
		return &StdoutError{Err: err}
	}
	return nil
}

func groupByArea(changed []string) map[string][]string {
	areas := map[string][]string{}
	for _, file := range changed {
		clean := filepath.ToSlash(filepath.Clean(file))
		area := "(root)"
		if i := strings.IndexByte(clean, '/'); i > 0 {
			area = clean[:i]
		}
		areas[area] = append(areas[area], file)
	}
	for _, files := range areas {
		sort.Strings(files)
	}
	return areas
}

func filesUnderRoots(roots, changed []string) []string {
	if len(roots) == 0 {
		return nil
	}
	var hits []string
	for _, file := range changed {
		if configboundary.ConfineToAny(roots, []string{file}) == nil {
			hits = append(hits, file)
		}
	}
	sort.Strings(hits)
	return hits
}

func readWatermark(path string) (docsWatermark, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return docsWatermark{}, false, nil
		}
		return docsWatermark{}, false, err
	}
	var wm docsWatermark
	if err := json.Unmarshal(data, &wm); err != nil {
		return docsWatermark{}, false, fmt.Errorf("parse watermark: %w", err)
	}
	if wm.SHA == "" || wm.RefreshedAt.IsZero() {
		return docsWatermark{}, false, fmt.Errorf("watermark %s is missing sha/refreshedAt", path)
	}
	return wm, true, nil
}

func writeWatermark(path string, wm docsWatermark) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(wm, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return durability.WriteFileAtomic(path, data, 0o644)
}

func revParse(git Git, repo, rev string) (string, error) {
	out, err := git(repo, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func boundaryCommit(git Git, repo string, sinceTime time.Time) (string, bool, error) {
	out, err := git(repo, "rev-list", "-1", "--before="+sinceTime.UTC().Format(time.RFC3339), "HEAD")
	if err != nil {
		return "", false, err
	}
	sha := strings.TrimSpace(out)
	return sha, sha != "", nil
}

func commitsInRange(git Git, repo, base, head string) ([]churnCommit, error) {
	const separator = "\x1e"
	format := "%H" + separator + "%s" + separator + "%b"
	rangeArg := base + ".." + head
	if base == emptyTreeObject {
		rangeArg = head
	}
	out, err := git(repo, "log", "-z", "--no-color", "--format="+format, rangeArg)
	if err != nil {
		return nil, err
	}
	var commits []churnCommit
	for _, record := range strings.Split(out, "\x00") {
		if strings.TrimSpace(record) == "" {
			continue
		}
		parts := strings.SplitN(record, separator, 3)
		if len(parts) < 2 {
			continue
		}
		commit := churnCommit{SHA: strings.TrimSpace(parts[0]), Subject: strings.TrimSpace(parts[1])}
		if len(parts) == 3 {
			commit.Body = strings.TrimSpace(parts[2])
		}
		commits = append(commits, commit)
	}
	return commits, nil
}

func changedFiles(git Git, repo, base, head string) ([]string, error) {
	out, err := git(repo, "diff", "--no-renames", "--name-only", "-z", base, head)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\x00") {
		if line != "" {
			files = append(files, line)
		}
	}
	sort.Strings(files)
	return files, nil
}
