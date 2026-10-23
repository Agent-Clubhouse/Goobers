package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/goobers/goobers/api/validate"
	"github.com/goobers/goobers/internal/configgeneration"
	"github.com/goobers/goobers/internal/instance"
)

// ErrUnstable means the directory changed while a candidate was being read.
// Nothing is applied; the next poll converges on the settled content.
var ErrUnstable = errors.New("cloud config directory changed while it was being read")

// DirSource builds candidates from a mounted, read-only instance directory:
// instance.yaml, the config/ definition tree and the optional sibling goobers/
// tree. The archive comes from the same configgeneration builder the local
// daemon retains generations with, so identical definitions yield the identical
// archive for the same instance identity.
type DirSource struct {
	// Root is the instance directory.
	Root string
	// InstanceID owns the archive: identical definitions from separate instances
	// must not share a deletable pin.
	InstanceID string
}

// resolved is one view of the mounted directory with every symlink followed.
// A Kubernetes ConfigMap or projected volume publishes ..data -> ..<timestamp>
// and symlinks each top-level entry through it, and swaps ..data atomically on
// update. The generation builder and the definition walker refuse symlinked
// roots (and the sibling goobers/ tree must stay beside config/), so each
// operation works on the resolved real directories of one swap.
type resolved struct {
	configDir    string
	instanceFile string
}

func (s DirSource) resolve() (resolved, error) {
	root, err := filepath.EvalSymlinks(s.Root)
	if err != nil {
		return resolved{}, fmt.Errorf("resolve config directory: %w", err)
	}
	l := instance.NewLayout(root)
	configDir, err := filepath.EvalSymlinks(l.ConfigDir())
	if err != nil {
		return resolved{}, fmt.Errorf("resolve config definitions: %w", err)
	}
	instanceFile, err := filepath.EvalSymlinks(l.ConfigFile())
	if err != nil {
		return resolved{}, fmt.Errorf("resolve instance config: %w", err)
	}
	return resolved{configDir: configDir, instanceFile: instanceFile}, nil
}

// Fingerprint is the cheap steady-state change signal: it covers everything a
// candidate is built from, instance.yaml included (the archive excludes it, but
// placement.selfExecution decides eligibility), and applies nothing.
func (s DirSource) Fingerprint(ctx context.Context) (string, error) {
	_, digest, err := s.capture(ctx)
	return digest, err
}

func (s DirSource) capture(ctx context.Context) (archive []byte, fingerprint string, err error) {
	view, err := s.resolve()
	if err != nil {
		return nil, "", err
	}
	archive, digest, err := configgeneration.CaptureForInstance(ctx, view.configDir, s.InstanceID)
	if err != nil {
		return nil, "", fmt.Errorf("capture config generation: %w", err)
	}
	cfg, err := os.ReadFile(view.instanceFile)
	if err != nil {
		return nil, "", fmt.Errorf("read instance config: %w", err)
	}
	sum := sha256.Sum256(append([]byte(digest+"\n"), cfg...))
	return archive, "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Load reads one whole, stable, valid view of the directory. A directory with
// validation errors is refused here (fail closed): it would silently drop
// workflows from the eligibility assessment.
func (s DirSource) Load(ctx context.Context) (Candidate, error) {
	if s.Root == "" || s.InstanceID == "" {
		return Candidate{}, errors.New("cloud config source requires a directory and an instance")
	}
	view, err := s.resolve()
	if err != nil {
		return Candidate{}, err
	}
	archive, before, err := s.capture(ctx)
	if err != nil {
		return Candidate{}, err
	}
	cfg, err := instance.LoadConfig(view.instanceFile)
	if err != nil {
		return Candidate{}, fmt.Errorf("load instance config: %w", err)
	}
	set, report, err := instance.LoadConfigDir(view.configDir)
	if err != nil {
		return Candidate{}, fmt.Errorf("load config definitions: %w%s", err, issueSummary(report))
	}
	if report != nil && report.HasErrors() {
		return Candidate{}, fmt.Errorf("config definitions have validation errors%s", issueSummary(report))
	}
	_, after, err := s.capture(ctx)
	if err != nil {
		return Candidate{}, err
	}
	if before != after {
		return Candidate{}, ErrUnstable
	}
	return Candidate{Digest: archiveDigest(archive), Archive: archive, Config: cfg, Set: set}, nil
}

// archiveDigest is the configgeneration content address of the archive bytes.
func archiveDigest(archive []byte) string {
	sum := sha256.Sum256(archive)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func issueSummary(report *validate.Report) string {
	if report == nil {
		return ""
	}
	var lines []string
	for _, issue := range report.Issues {
		if issue.Severity == validate.Error {
			lines = append(lines, issue.File+": "+issue.Message)
		}
		if len(lines) == 5 {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return " (" + strings.Join(lines, "; ") + ")"
}

// DefaultPollInterval is how often a mounted directory is re-examined.
const DefaultPollInterval = 30 * time.Second

// DirConfiguration is the control role's Configuration over a mounted
// directory. Reload is called at epoch start and on every controller tick, so
// it is throttled to the poll interval and does nothing at all while the
// directory's content digest is unchanged. A candidate refused as ineligible is
// remembered by directory digest and not re-assessed until the content changes;
// the running generation and its guard stay in force and Status names the
// blockers.
type DirConfiguration struct {
	*Configuration
	source   DirSource
	interval time.Duration
	now      func() time.Time

	mu      sync.Mutex
	seen    string // directory digest last applied or refused
	checked time.Time
	report  func(error)
}

// NewDirConfiguration returns a Configuration over source. It reads the
// directory once, without applying, so a missing or unreadable directory fails
// at boot rather than at the first tick.
func NewDirConfiguration(assess Assessor, activator Activator, source DirSource) (*DirConfiguration, error) {
	if _, err := source.Load(context.Background()); err != nil && !errors.Is(err, ErrUnstable) {
		return nil, fmt.Errorf("cloud config directory %q: %w", source.Root, err)
	}
	inner, err := NewConfiguration(assess, activator, func(ctx context.Context) (Candidate, error) {
		candidate, err := source.Load(ctx)
		if err != nil && !errors.Is(err, ErrUnstable) {
			return candidate, &loadError{err}
		}
		return candidate, err
	})
	if err != nil {
		return nil, err
	}
	return &DirConfiguration{Configuration: inner, source: source, interval: DefaultPollInterval, now: time.Now, report: func(error) {}}, nil
}

// loadError marks a failure to read or validate the directory, as opposed to a
// failure to publish an assessed candidate.
type loadError struct{ err error }

func (e *loadError) Error() string { return e.err.Error() }
func (e *loadError) Unwrap() error { return e.err }

// SetReporter installs the sink for refusals and unstable reads. nil clears it.
func (d *DirConfiguration) SetReporter(report func(error)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if report == nil {
		report = func(error) {}
	}
	d.report = report
}

// Reload applies the directory when its content changed since the last look.
func (d *DirConfiguration) Reload(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.checked.IsZero() && d.now().Sub(d.checked) < d.interval {
		return nil
	}
	d.checked = d.now()
	digest, err := d.source.Fingerprint(ctx)
	if err != nil {
		// Unreadable is the same class as invalid: the running generation stays,
		// the problem is reported, and the next poll re-reads. A read failure must
		// not fail the controller tick.
		d.report(fmt.Errorf("digest config directory: %w", err))
		return nil
	}
	if digest == d.seen {
		return nil
	}
	err = d.Configuration.Reload(ctx)
	var refusal *Refusal
	switch {
	case err == nil:
		d.seen = digest
	case errors.As(err, &refusal):
		// Deterministic for this content: remember it, report it, and keep the
		// running generation. Returning the refusal would fail the epoch's tick
		// over a config problem the operator fixes by editing the directory.
		d.seen = digest
		d.report(err)
		return nil
	case errors.Is(err, ErrUnstable):
		d.checked = time.Time{}
		d.report(err)
		return nil
	case errors.As(err, new(*loadError)):
		// An invalid or unreadable directory is not the controller's failure: the
		// running generation stays in force (and before the first apply nothing
		// admits), the problem is reported, and the next poll re-reads it. It is
		// not remembered, so a transient read error cannot stick.
		d.report(err)
		return nil
	default:
		return err
	}
	return nil
}
