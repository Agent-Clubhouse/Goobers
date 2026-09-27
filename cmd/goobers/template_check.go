package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goobers/goobers/internal/gaggletemplate"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/platform/lock"
)

const templateCheckInterval = 24 * time.Hour

func runTemplateCheck(args []string, stdout, stderr io.Writer) int {
	return runTemplateInspection(true, args, stdout, stderr)
}

func runTemplateStatus(args []string, stdout, stderr io.Writer) int {
	return runTemplateInspection(false, args, stdout, stderr)
}

func runTemplateInspection(check bool, args []string, stdout, stderr io.Writer) int {
	name := "status"
	if check {
		name = "check"
	}
	flags := newCLIFlagSet("config templates "+name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = helpUsage(stderr, "config templates "+name)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 1 {
		flags.Usage()
		return 2
	}
	root := "."
	if flags.NArg() == 1 {
		root = flags.Arg(0)
	}
	if _, err := instance.LoadConfig(instance.NewLayout(root).ConfigFile()); err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	if !check {
		reportTemplateStatus(root, stdout)
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err := checkGaggleTemplates(ctx, root, time.Now().UTC())
	reportTemplateStatus(root, stdout)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

func templateGaggles(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(instance.NewLayout(root).ConfigDir(), "gaggles"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(instance.NewLayout(root).ConfigDir(), "gaggles", entry.Name(), gaggletemplate.MetadataDir)
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return nil, err
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

func checkGaggleTemplates(ctx context.Context, root string, now time.Time) ([]string, error) {
	names, err := templateGaggles(root)
	if err != nil || len(names) == 0 {
		return nil, err
	}
	cache := filepath.Join(root, "template-status")
	if err := os.MkdirAll(cache, 0755); err != nil {
		return nil, err
	}
	held, err := lock.TryAcquire(filepath.Join(cache, "check.lock"))
	if err != nil {
		return nil, fmt.Errorf("template check already running or unavailable: %w", err)
	}
	defer func() { _ = held.Release() }()
	var notices []string
	var failures []error
	for _, name := range names {
		prior, priorErr := gaggletemplate.ReadStatus(root, name)
		status := checkOneTemplate(ctx, root, name, now)
		if priorErr != nil && !errors.Is(priorErr, fs.ErrNotExist) {
			failures = append(failures, fmt.Errorf("read prior template status for %s: %w", name, priorErr))
		}
		if prior != nil && status.LastSuccess.IsZero() {
			status.LastSuccess = prior.LastSuccess
		}
		if err := gaggletemplate.WriteStatus(root, name, status); err != nil {
			failures = append(failures, err)
			continue
		}
		if status.Error != "" {
			failures = append(failures, fmt.Errorf("%s: %s", name, status.Error))
		}
		if prior == nil || prior.State != status.State || prior.CandidateDigest != status.CandidateDigest ||
			prior.Error != status.Error || prior.PendingBackprop != status.PendingBackprop {
			notices = append(notices, templateNotice(name, status))
		}
	}
	return notices, errors.Join(failures...)
}

func checkOneTemplate(ctx context.Context, root, name string, now time.Time) gaggletemplate.Status {
	status := gaggletemplate.Status{State: "unknown", CheckedAt: now}
	if err := populateTemplateCheck(ctx, root, name, &status); err != nil {
		status.State, status.Error = "unknown", err.Error()
	} else {
		status.LastSuccess = now
	}
	return status
}

func populateTemplateCheck(ctx context.Context, root, name string, status *gaggletemplate.Status) error {
	directory := filepath.Join(instance.NewLayout(root).ConfigDir(), "gaggles", name)
	tracking, err := requiredTemplate(directory)
	if err != nil {
		return err
	}
	status.Installed = tracking.Lock.Revision
	local, err := gaggletemplate.ReadTree(directory)
	if err != nil {
		return err
	}
	deployed, err := gaggletemplate.Deployment(directory)
	if err != nil {
		return err
	}
	status.PendingBackprop = !gaggletemplate.Equivalent(deployed, local)
	upstream, revision, err := resolveGaggleTemplate(ctx, root, tracking.Source, tracking.Lock.Revision)
	if err != nil {
		return err
	}
	status.Candidate = revision
	status.CandidateDigest = upstream.Digest()
	status.Changes = gaggletemplate.Changes(tracking.Lock.Baseline, upstream)
	status.State = "current"
	if len(status.Changes) > 0 {
		status.State = "update-available"
		_, conflicts, err := gaggletemplate.Merge(tracking.Lock.Baseline, local, upstream)
		if err != nil {
			return err
		}
		status.Conflicts = conflicts
		if len(conflicts) > 0 {
			status.State = "conflicts"
		}
	}
	return nil
}

func templateNotice(name string, status gaggletemplate.Status) string {
	message := fmt.Sprintf("template %s: %s", name, status.State)
	if status.State == "update-available" || status.State == "conflicts" {
		message += fmt.Sprintf(" (%d changed files); review with goobers config templates update --gaggle %s", len(status.Changes), name)
	}
	if status.PendingBackprop {
		message += "; runtime edits pending backprop"
	}
	if status.Error != "" {
		message += "; " + status.Error
	}
	return message
}

func reportTemplateStatus(root string, stdout io.Writer) {
	names, err := templateGaggles(root)
	if err != nil {
		pf(stdout, "template status unavailable: %v\n", err)
		return
	}
	for _, name := range names {
		status := gaggletemplate.InventoryStatus(root, name)
		if status == nil {
			continue
		}
		pln(stdout, templateNotice(name, *status))
		if status.Installed != "" {
			pf(stdout, "  installed revision: %s\n", status.Installed)
		}
		if status.Candidate != "" {
			pf(stdout, "  source revision: %s\n", status.Candidate)
		}
		if status.LastSuccess.IsZero() {
			pln(stdout, "  no successful source check")
		} else {
			pf(stdout, "  last successful check: %s\n", status.LastSuccess.Format(time.RFC3339))
			if time.Since(status.LastSuccess) > templateCheckInterval {
				pln(stdout, "  stale: source has not been checked in the last 24 hours")
			}
		}
		for _, conflict := range status.Conflicts {
			pf(stdout, "  conflict: %s\n", conflict)
		}
		for _, change := range status.Changes {
			pf(stdout, "  changed: %s\n", change)
		}
	}
}

func startTemplateChecks(ctx context.Context, root string) (<-chan updateCheckResult, <-chan struct{}) {
	results := make(chan updateCheckResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(templateCheckInterval)
		defer ticker.Stop()
		for {
			checkCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			notices, err := checkGaggleTemplates(checkCtx, root, time.Now().UTC())
			cancel()
			result := updateCheckResult{notice: strings.Join(notices, "\n")}
			if err != nil {
				result.warning = "template check: " + err.Error()
			}
			if result.notice != "" || result.warning != "" {
				select {
				case results <- result:
				case <-ctx.Done():
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return results, done
}
