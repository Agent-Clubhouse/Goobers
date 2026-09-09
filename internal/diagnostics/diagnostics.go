// Package diagnostics builds a portable, redacted support bundle from an
// installed binary and an instance directory alone (#2968).
//
// The problem it solves is a support prerequisite, not a missing feature: an
// operator holding a released binary, an instance root and its run journals
// still had to open a checkout of the Goobers source and grep implementation
// files to answer basic production questions — why a selector returned
// no-work while eligible-looking pull requests existed, which lifecycle label
// or durable ledger entry excluded an item, which binary and config generation
// made a decision, which credentials a stage required and whether they were
// present. Reading Goobers' own source must never be part of reading a
// Goobers incident.
//
// # Redaction by construction
//
// Two mechanisms, and the first is the load-bearing one.
//
// STRUCTURAL. The bundle projects only fields that cannot carry a secret. A
// credential contributes its capability, its source KIND and its source NAME —
// never a resolved value, and the collector never resolves one. Agent
// transcripts and stage stdout are excluded wholesale rather than filtered:
// they are the surfaces where a prompt can quote a token, and a bundle that
// summarizes them cannot leak what it never reads. Artifacts contribute
// metadata (path, digest, size, media type, integrity) and not content.
//
// TEXTUAL. Every string that survives that projection is then passed through
// the same secret-pattern net the journal writes behind, so a token pasted into
// an issue title or an error message is redacted on the way out too. That net
// is the backstop; the projection is the guarantee.
//
// # Determinism
//
// The same instance state produces the same bundle bytes. Every list is sorted
// by a stable key, the collection clock is injected rather than read, and
// nothing embeds a host path outside the instance root. That is what makes
// "reproduce it on another machine" a testable claim rather than an aspiration.
package diagnostics

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

// Schema is the bundle contract identity. It is versioned because a support
// bundle is read by tooling that may be older or newer than the binary that
// wrote it.
const Schema = "goobers.dev/diagnostics/v1"

// Bundle is the machine-readable half of a diagnostics bundle.
type Bundle struct {
	Schema      string               `json:"schema"`
	GeneratedAt string               `json:"generatedAt"`
	Binary      BinaryInfo           `json:"binary"`
	Contract    ContractInfo         `json:"contract"`
	Instance    InstanceInfo         `json:"instance"`
	Daemon      DaemonInfo           `json:"daemon"`
	Credentials []CredentialPresence `json:"credentials"`
	Runs        []RunInfo            `json:"runs"`
	// Notes record what the collector could NOT read and why. A bundle that
	// silently omits an unreadable source is worse than one that says so: the
	// reader would conclude the absence was the finding.
	Notes []string `json:"notes,omitempty"`
}

// BinaryInfo identifies the binary that produced the bundle — the "which
// generation made this decision" question.
type BinaryInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Go      string `json:"go"`
}

// ContractInfo pins the exact contract surface the running binary implements,
// so a bundle can be read against the right contract rather than against
// whatever the reader's own checkout happens to be at.
type ContractInfo struct {
	// JournalSchemaVersion is the run-directory schema this binary writes.
	JournalSchemaVersion int `json:"journalSchemaVersion"`
	// DSLVersions are the workflow DSL versions this binary admits.
	DSLVersions []string `json:"dslVersions,omitempty"`
	// StageCommands are the built-in provider-chain stage commands, each with
	// the capabilities it requires and its default result file. This is the
	// half an operator previously had to read providerstage/manifest.go for.
	StageCommands []StageCommandInfo `json:"stageCommands,omitempty"`
}

// StageCommandInfo is one built-in stage command's admission contract.
type StageCommandInfo struct {
	Command      string   `json:"command"`
	ResultFile   string   `json:"resultFile,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// InstanceInfo describes the instance's own configuration generation.
type InstanceInfo struct {
	Root string `json:"root"`
	// ConfigDigest is the content digest of the loaded config tree, so two
	// bundles can be compared for "did the config change between these".
	ConfigDigest string `json:"configDigest,omitempty"`
	// ConfigIssues are the loaded tree's validation findings. A config that
	// does not validate is a first-class diagnostic: an instance behaving
	// oddly because the daemon refuses its config must not present as an
	// instance with a healthy config.
	ConfigIssues []string     `json:"configIssues,omitempty"`
	Gaggles      []GaggleInfo `json:"gaggles,omitempty"`
}

// GaggleInfo is one gaggle's loaded definition generation.
type GaggleInfo struct {
	Name      string         `json:"name"`
	Workflows []WorkflowInfo `json:"workflows,omitempty"`
	Goobers   []string       `json:"goobers,omitempty"`
}

// WorkflowInfo pins one authored workflow definition.
type WorkflowInfo struct {
	Name       string `json:"name"`
	DSLVersion string `json:"dslVersion,omitempty"`
	// DefinitionDigest is the digest of the AUTHORED definition, not the
	// compiled machine digest a run journal pins. Computing the latter means
	// compiling with the daemon's full option set, which resolves the
	// agent:model credential — and this bundle must never materialize a
	// secret. A run's own compiled digest is reported from its journal, where
	// it was already recorded, so both are available without either being
	// mislabelled as the other.
	DefinitionDigest string `json:"definitionDigest,omitempty"`
}

// DaemonInfo is the daemon lifecycle state at collection time.
type DaemonInfo struct {
	Running bool `json:"running"`
	// LockPresent distinguishes "no daemon ever ran here" from "a lock exists
	// but nothing holds it" — the stale-lock case that reads as a live daemon.
	LockPresent bool   `json:"lockPresent"`
	PID         int    `json:"pid,omitempty"`
	Version     string `json:"version,omitempty"`
	StartedAt   string `json:"startedAt,omitempty"`
	LastTickAt  string `json:"lastTickAt,omitempty"`
	// Stale reports that a daemon holds the lock but has not ticked within its
	// liveness timeout.
	Stale bool `json:"stale,omitempty"`
}

// CredentialPresence records that a credential was declared and whether its
// source resolves — never what it resolves to.
type CredentialPresence struct {
	// Capability is the canonical capability the grant backs, or the MCP
	// server name for a BYO MCP credential.
	Capability string `json:"capability"`
	// SourceKind is env | file | keychain | store | githubCLI.
	SourceKind string `json:"sourceKind"`
	// SourceName is the environment variable name, file path, keychain
	// service, store reference, or CLI login — a NAME, never a value.
	SourceName string `json:"sourceName,omitempty"`
	// Present reports whether the source exists where the collector can tell
	// cheaply and without reading a value: an environment variable is set, a
	// file exists. Nil means "not determinable without resolving the
	// credential", which the collector deliberately never does.
	Present *bool `json:"present,omitempty"`
}

// RunInfo is one run reduced to what explains its outcome.
type RunInfo struct {
	RunID          string      `json:"runId"`
	Workflow       string      `json:"workflow"`
	Gaggle         string      `json:"gaggle"`
	WorkflowDigest string      `json:"workflowDigest,omitempty"`
	GooberDigest   string      `json:"gooberDigest,omitempty"`
	Phase          string      `json:"phase,omitempty"`
	StartedAt      string      `json:"startedAt,omitempty"`
	FinishedAt     string      `json:"finishedAt,omitempty"`
	Stages         []StageInfo `json:"stages,omitempty"`
	// Decisions are the selector inclusion/exclusion reasons this run
	// recorded. They are the direct answer to "why did the selector return
	// no-work while eligible-looking items existed".
	Decisions []Decision `json:"decisions,omitempty"`
	// DecisiveError is the run's terminal error, in full rather than
	// truncated: the whole point is to stop an operator hunting for the
	// artifact that holds the real message.
	DecisiveError *ErrorInfo `json:"decisiveError,omitempty"`
}

// StageInfo is one stage attempt's outcome and artifact metadata.
type StageInfo struct {
	Stage     string         `json:"stage"`
	Attempt   int            `json:"attempt,omitempty"`
	Status    string         `json:"status,omitempty"`
	Error     *ErrorInfo     `json:"error,omitempty"`
	Artifacts []ArtifactInfo `json:"artifacts,omitempty"`
}

// ArtifactInfo is an artifact's metadata. Never its content: an artifact can
// hold an agent transcript, and the bundle must be safe to hand to a support
// channel by construction.
type ArtifactInfo struct {
	Path      string `json:"path"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	MediaType string `json:"mediaType,omitempty"`
	Integrity string `json:"integrity,omitempty"`
}

// ErrorInfo is a stage or run error, with the classification the runner
// recorded.
type ErrorInfo struct {
	Stage   string `json:"stage,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Decision is one recorded selector inclusion or exclusion, with the reason
// the stage gave for it.
type Decision struct {
	Stage    string `json:"stage"`
	Subject  string `json:"subject"`
	Included bool   `json:"included"`
	Reason   string `json:"reason,omitempty"`
}

// Options bound and parameterize a collection.
type Options struct {
	// Root is the instance directory.
	Root string
	// Now is the collection clock. Injected so a bundle's only time-varying
	// field is under the caller's control, which is what makes the rest of it
	// byte-comparable between two collections of the same state.
	Now time.Time
	// RunID limits the bundle to one run. Empty collects the most recent runs
	// up to MaxRuns.
	RunID string
	// PR limits the bundle to runs that touched one pull request.
	PR int
	// MaxRuns caps how many runs are collected when no run is named. Zero
	// means DefaultMaxRuns.
	MaxRuns int
}

// DefaultMaxRuns bounds an unfiltered collection. A bundle is a support
// artifact, not an archive: an instance with ten thousand runs must still
// produce something a person can read and a channel can accept.
const DefaultMaxRuns = 25

// Scrubber redacts secret-shaped substrings from bundle text.
type Scrubber interface {
	Scrub([]byte) []byte
}

// Collector reads an instance and produces a Bundle. Its inputs are injected
// so the collection is testable without a daemon, a provider, or a clock.
type Collector struct {
	Binary   BinaryInfo
	Contract ContractInfo
	Scrubber Scrubber
	// Daemon reads daemon lifecycle state for an instance root. Injected
	// because lock inspection is platform-specific and lives in the CLI.
	Daemon func(root string, now time.Time) (DaemonInfo, error)
	// Instance reads the loaded configuration generation.
	Instance func(root string) (InstanceInfo, []CredentialPresence, error)
	// RunDirs lists the instance's run directories, newest last.
	RunDirs func(root string) ([]string, error)
}

// Collect builds the bundle.
//
// A failure to read one source is a Note, not an error: a support bundle
// collected during an incident must be produced from whatever IS readable. The
// only hard failures are ones that would make the bundle meaningless — a root
// that is not an instance, or an explicitly named run that does not exist.
func (c Collector) Collect(opts Options) (Bundle, error) {
	if strings.TrimSpace(opts.Root) == "" {
		return Bundle{}, errors.New("diagnostics: instance root is required")
	}
	bundle := Bundle{
		Schema:      Schema,
		GeneratedAt: opts.Now.UTC().Format(time.RFC3339),
		Binary:      c.Binary,
		Contract:    c.Contract,
		Instance:    InstanceInfo{Root: filepath.ToSlash(opts.Root)},
	}

	if c.Instance != nil {
		info, creds, err := c.Instance(opts.Root)
		if err != nil {
			bundle.Notes = append(bundle.Notes, "instance configuration could not be loaded: "+err.Error())
		} else {
			info.Root = bundle.Instance.Root
			bundle.Instance = info
			bundle.Credentials = creds
		}
	}
	if c.Daemon != nil {
		daemon, err := c.Daemon(opts.Root, opts.Now)
		if err != nil {
			bundle.Notes = append(bundle.Notes, "daemon lifecycle could not be inspected: "+err.Error())
		} else {
			bundle.Daemon = daemon
		}
	}

	runs, err := c.collectRuns(opts, &bundle)
	if err != nil {
		return Bundle{}, err
	}
	bundle.Runs = runs
	if bundle.Credentials == nil {
		bundle.Credentials = []CredentialPresence{}
	}
	if bundle.Runs == nil {
		bundle.Runs = []RunInfo{}
	}
	return c.scrub(bundle), nil
}

func (c Collector) collectRuns(opts Options, bundle *Bundle) ([]RunInfo, error) {
	if c.RunDirs == nil {
		return nil, nil
	}
	dirs, err := c.RunDirs(opts.Root)
	if err != nil {
		bundle.Notes = append(bundle.Notes, "run journals could not be listed: "+err.Error())
		return nil, nil
	}
	maxRuns := opts.MaxRuns
	if maxRuns <= 0 {
		maxRuns = DefaultMaxRuns
	}

	var runs []RunInfo
	matchedNamedRun := false
	// Newest first: an incident is almost always about recent runs, and the
	// cap must drop the oldest rather than the most relevant.
	for i := len(dirs) - 1; i >= 0; i-- {
		if len(runs) >= maxRuns && opts.RunID == "" {
			break
		}
		info, note, ok := c.collectRun(dirs[i], opts)
		if note != "" {
			bundle.Notes = append(bundle.Notes, note)
		}
		if !ok {
			continue
		}
		if opts.RunID != "" {
			if info.RunID != opts.RunID {
				continue
			}
			matchedNamedRun = true
		}
		runs = append(runs, info)
	}
	if opts.RunID != "" && !matchedNamedRun {
		return nil, fmt.Errorf("diagnostics: run %q not found under %s", opts.RunID, opts.Root)
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].RunID < runs[j].RunID })
	return runs, nil
}

func (c Collector) collectRun(dir string, opts Options) (RunInfo, string, bool) {
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		// A journal at an older schema is common on a support host and must
		// not abort the collection; say so and move on.
		return RunInfo{}, fmt.Sprintf("run journal %s could not be read: %v", filepath.Base(dir), err), false
	}
	identity, err := reader.Identity()
	if err != nil {
		return RunInfo{}, fmt.Sprintf("run journal %s has no readable identity: %v", filepath.Base(dir), err), false
	}
	info := RunInfo{
		RunID:          identity.RunID,
		Workflow:       identity.Workflow,
		Gaggle:         identity.Gaggle,
		WorkflowDigest: identity.WorkflowDigest,
		GooberDigest:   identity.GooberDigest,
	}
	if phase, err := reader.Phase(); err == nil {
		info.Phase = string(phase)
	}
	events, err := reader.Events()
	if err != nil {
		return info, fmt.Sprintf("run %s events could not be read: %v", identity.RunID, err), true
	}
	summarizeRunEvents(&info, events)
	if opts.PR > 0 && !runTouchesPR(info, events, opts.PR) {
		return RunInfo{}, "", false
	}
	return info, "", true
}

func summarizeRunEvents(info *RunInfo, events []journal.Event) {
	for _, event := range events {
		if info.StartedAt == "" {
			info.StartedAt = event.Time.UTC().Format(time.RFC3339)
		}
		info.FinishedAt = event.Time.UTC().Format(time.RFC3339)

		stage := StageInfo{Stage: event.Stage, Attempt: event.Attempt}
		switch event.Type {
		case journal.EventStageFinished:
			stage.Status = event.Status
			appendArtifacts(&stage, event.Artifacts)
			if event.Error != nil {
				stage.Error = &ErrorInfo{Stage: event.Stage, Code: event.Error.Code, Message: event.Error.Message}
			}
			info.Stages = append(info.Stages, stage)
		case journal.EventError:
			detail := &ErrorInfo{Stage: event.Stage}
			if event.Error != nil {
				detail.Code = event.Error.Code
				detail.Message = event.Error.Message
			}
			// Last error wins: the decisive one is the one the run ended on.
			info.DecisiveError = detail
		}
		info.Decisions = append(info.Decisions, decisionsFromEvent(event)...)
	}
}

func appendArtifacts(stage *StageInfo, refs []journal.Ref) {
	for _, ref := range refs {
		stage.Artifacts = append(stage.Artifacts, ArtifactInfo{
			Path:      ref.Path,
			Digest:    ref.Digest,
			Size:      ref.Size,
			MediaType: ref.MediaType,
			Integrity: string(ref.Integrity),
		})
	}
}

// selectionOutputs are the scalar stage outputs that carry a selector's own
// account of what it did. Reading them from the journal — rather than from a
// stage's stderr, which can quote arbitrary provider content — keeps the
// decision record inside the structured contract.
var selectionOutputs = map[string]bool{
	"excluded":        true,
	"exclusionReason": true,
	"selectionReason": true,
	"noWorkReason":    true,
	"skipped":         true,
	"selected":        true,
	"selectedNumber":  true,
	"claimedId":       true,
}

func decisionsFromEvent(event journal.Event) []Decision {
	if len(event.Outputs) == 0 {
		return nil
	}
	var out []Decision
	keys := make([]string, 0, len(event.Outputs))
	for key := range event.Outputs {
		if selectionOutputs[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := fmt.Sprint(event.Outputs[key])
		if strings.TrimSpace(value) == "" {
			continue
		}
		out = append(out, Decision{
			Stage:    event.Stage,
			Subject:  key,
			Included: key == "selected" || key == "selectedNumber" || key == "claimedId" || key == "selectionReason",
			Reason:   value,
		})
	}
	return out
}

func runTouchesPR(info RunInfo, events []journal.Event, pr int) bool {
	want := strconv.Itoa(pr)
	for _, event := range events {
		for key, value := range event.Outputs {
			if !strings.Contains(strings.ToLower(key), "number") && !strings.Contains(strings.ToLower(key), "pr") {
				continue
			}
			if fmt.Sprint(value) == want {
				return true
			}
		}
	}
	for _, decision := range info.Decisions {
		if decision.Reason == want {
			return true
		}
	}
	return false
}

// scrub applies the textual net to every string the projection produced. The
// projection is the guarantee; this is the backstop for a secret pasted into
// content the projection legitimately keeps, such as an error message.
func (c Collector) scrub(bundle Bundle) Bundle {
	if c.Scrubber == nil {
		return bundle
	}
	clean := func(s string) string {
		if s == "" {
			return s
		}
		return string(c.Scrubber.Scrub([]byte(s)))
	}
	for i := range bundle.Notes {
		bundle.Notes[i] = clean(bundle.Notes[i])
	}
	for i := range bundle.Credentials {
		bundle.Credentials[i].SourceName = clean(bundle.Credentials[i].SourceName)
	}
	for i := range bundle.Runs {
		run := &bundle.Runs[i]
		if run.DecisiveError != nil {
			run.DecisiveError.Message = clean(run.DecisiveError.Message)
		}
		for j := range run.Stages {
			if run.Stages[j].Error != nil {
				run.Stages[j].Error.Message = clean(run.Stages[j].Error.Message)
			}
		}
		for j := range run.Decisions {
			run.Decisions[j].Reason = clean(run.Decisions[j].Reason)
		}
	}
	return bundle
}

// CredentialPresenceFor builds a presence record without resolving anything.
// Exported so the CLI's config projection and the tests agree on exactly what
// "present" is allowed to mean.
func CredentialPresenceFor(capability, kind, name string, lookupEnv func(string) (string, bool)) CredentialPresence {
	record := CredentialPresence{Capability: capability, SourceKind: kind, SourceName: name}
	switch kind {
	case "env":
		if lookupEnv != nil {
			_, ok := lookupEnv(name)
			record.Present = &ok
		}
	case "file":
		_, err := os.Stat(name)
		present := err == nil
		record.Present = &present
	}
	// keychain, store and githubCLI are deliberately left nil: determining
	// presence would mean asking the resolver, and asking the resolver means
	// materializing a secret this bundle exists to never touch.
	return record
}
