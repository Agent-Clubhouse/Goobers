package main

import (
	"context"
	"flag"
	"io"
	"path/filepath"

	"github.com/goobers/goobers/internal/instance"
)

// apply.go implements #459: an explicit, one-shot "reconcile now" for a live
// `goobers up` daemon, for an operator who doesn't want to wait for
// --watch-config's poll interval (or who isn't running with it at all). It
// pulls the instance's configured workflowSource (a git-tracked ref, or the
// local config directory itself) and swaps to the new valid config
// immediately, or reports the validation failure and keeps the daemon on its
// last-known-good definitions — the identical contract configReloader
// already enforces for a human hand-editing files in place.
//
// Reuses the #831 cancel-request file protocol (runcancel.go /
// applyrequest.go) rather than inventing a third request/response mechanism:
// this is a short-lived CLI process handing a request to the long-lived
// daemon and waiting for its answer, exactly like `goobers run cancel`.

const applyHelp = "Usage: goobers apply [path]\n\n" +
	"Ask a live `goobers up` daemon to reconcile its workflow definitions\n" +
	"now, instead of waiting for the configured source's poll interval. For\n" +
	"a git-tracked workflowSource, first pulls\n" +
	"the tracked ref's latest commit; for a local-dir source, just forces an\n" +
	"immediate validate-and-reload of the config directory as it stands.\n\n" +
	"On success the daemon's live definitions swap to the new commit/edit\n" +
	"immediately. An invalid pulled/edited config is rejected: the daemon\n" +
	"keeps running its last-known-good definitions, and this command reports\n" +
	"the validation failure. Exit codes: 0 = applied or already current,\n" +
	"1 = no live daemon found or the pulled/edited config was rejected,\n" +
	"2 = usage/IO error.\n"

func runApply(args []string, stdout, stderr io.Writer) int {
	fs := newCLIFlagSet("apply", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = helpUsage(stderr, "apply")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fs.Usage()
		return 2
	}
	root := "."
	if fs.NArg() == 1 {
		root = fs.Arg(0)
	}

	l := instance.NewLayout(root)
	running, _, err := inspectDaemonLock(filepath.Join(l.SchedulerDir(), "up.lock"))
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	if !running {
		pf(stderr, "error: no live `goobers up` daemon found for %s\n", root)
		return 1
	}

	requestID, err := writeApplyRequest(l.SchedulerDir())
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}
	resp, err := pollApplyResponse(context.Background(), l.SchedulerDir(), requestID, applyDelegationTimeout)
	if err != nil {
		pf(stderr, "error: %v\n", err)
		return 2
	}

	switch {
	case resp.Error != "":
		pf(stderr, "error: %s\n", resp.Error)
		return 1
	case resp.Rejected != "":
		pf(stderr, "error: config rejected, keeping last-known-good definitions: %s\n", resp.Rejected)
		return 1
	case resp.Applied:
		if resp.Revision != "" {
			pf(stdout, "applied %s -> %s (revision %s)\n", resp.OldDigest, resp.NewDigest, resp.Revision)
		} else {
			pf(stdout, "applied %s -> %s\n", resp.OldDigest, resp.NewDigest)
		}
		return 0
	default:
		pf(stdout, "already current (digest %s); nothing to apply\n", resp.OldDigest)
		return 0
	}
}
