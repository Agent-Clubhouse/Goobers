package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"time"

	hashiversion "github.com/hashicorp/go-version"
)

const (
	// ChannelStable tracks the newest stable release, skipping pre-releases.
	ChannelStable = "stable"
	// ChannelPrerelease tracks the newest release including pre-releases.
	ChannelPrerelease = "prerelease"

	checkSchema = "goobers.dev/update-check/v1"

	// DefaultCheckInterval is how often the daemon re-checks for a newer
	// release when the operator does not configure an interval.
	DefaultCheckInterval = 24 * time.Hour
	// DefaultCheckTimeout bounds a single release check. The check runs off
	// the daemon's critical path, so this only bounds how long a stuck
	// request holds its goroutine.
	DefaultCheckTimeout = 15 * time.Second
)

// ErrVersionNotComparable reports that the running build carries no SemVer
// version to compare against — the `dev` default every plain `go build`
// produces. It is not a failure: there is simply nothing to say. Callers
// notify on no other error and stay silent on this one.
var ErrVersionNotComparable = errors.New("running build has no comparable version")

// CheckOptions configures a notify-only release check. It deliberately shares
// nothing with the staging path: a check reads release metadata and never
// downloads, stages, or activates a binary.
type CheckOptions struct {
	// CurrentVersion is the running build's version (version.Get().Version).
	CurrentVersion string
	// Owner and Repository locate the product release source. Empty defaults
	// to the canonical Goobers product repository, independent of whatever
	// workload repositories the instance operates on.
	Owner, Repository string
	// Channel is ChannelStable (default) or ChannelPrerelease.
	Channel string
	// Token optionally authenticates the release query to raise the
	// unauthenticated rate limit. A check works without one.
	Token string

	APIBaseURL string
	HTTPClient *http.Client
}

// CheckResult is the outcome of a release check. It is also the on-disk cache
// shape `goobers status` renders, so the daemon is the only process that makes
// the request.
type CheckResult struct {
	Schema string `json:"schema"`
	// CurrentVersion is the build that performed the check.
	CurrentVersion string `json:"currentVersion"`
	// LatestVersion is the newest release tag the channel resolved to.
	LatestVersion string `json:"latestVersion"`
	// UpdateAvailable reports LatestVersion > CurrentVersion by SemVer.
	UpdateAvailable bool `json:"updateAvailable"`
	// Channel is the channel the check resolved through.
	Channel string `json:"channel"`
	// CheckedAt is when the check completed.
	CheckedAt time.Time `json:"checkedAt"`
}

// CheckLatest resolves the newest release on the configured channel and
// compares it with the running build. It performs one GET against the product
// repository's releases and never writes to the instance.
func CheckLatest(ctx context.Context, opts CheckOptions) (CheckResult, error) {
	opts = defaultCheckOptions(opts)
	if err := validateChannel(opts.Channel); err != nil {
		return CheckResult{}, err
	}
	// A `dev` build (every plain `go build`) carries no SemVer to compare, so
	// resolve nothing and make no request at all: a developer's daemon must
	// not announce an "update" on every start.
	current, err := hashiversion.NewVersion(opts.CurrentVersion)
	if err != nil {
		return CheckResult{}, fmt.Errorf("%w: %q", ErrVersionNotComparable, opts.CurrentVersion)
	}
	release, _, err := resolveRelease(ctx, opts.prepareOptions())
	if err != nil {
		return CheckResult{}, err
	}
	latest, err := hashiversion.NewVersion(release.TagName)
	if err != nil {
		return CheckResult{}, fmt.Errorf(
			"product repository %s/%s returned non-SemVer release tag %q: %w",
			opts.Owner, opts.Repository, release.TagName, err)
	}
	return CheckResult{
		Schema:          checkSchema,
		CurrentVersion:  opts.CurrentVersion,
		LatestVersion:   release.TagName,
		UpdateAvailable: latest.GreaterThan(current),
		Channel:         opts.Channel,
		CheckedAt:       time.Now().UTC(),
	}, nil
}

// prepareOptions projects the check's inputs onto the staging path's option
// struct so release resolution has exactly one implementation. The channel
// maps onto the same on-release policy `self-update` stages through, so
// "stable" and "prerelease" can never mean something different here than they
// mean when the operator acts on the notice.
func (o CheckOptions) prepareOptions() PrepareOptions {
	return PrepareOptions{
		Policy:            PolicyOnRelease,
		IncludePrerelease: o.Channel == ChannelPrerelease,
		Owner:             o.Owner,
		Repository:        o.Repository,
		Token:             o.Token,
		APIBaseURL:        o.APIBaseURL,
		HTTPClient:        o.HTTPClient,
	}
}

func defaultCheckOptions(opts CheckOptions) CheckOptions {
	opts.Channel = valueOr(opts.Channel, ChannelStable)
	opts.Owner = valueOr(opts.Owner, DefaultProductOwner)
	opts.Repository = valueOr(opts.Repository, DefaultProductRepository)
	opts.APIBaseURL = valueOr(opts.APIBaseURL, defaultAPIBaseURL)
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: DefaultCheckTimeout}
	}
	return opts
}

func validateChannel(channel string) error {
	switch channel {
	case ChannelStable, ChannelPrerelease:
		return nil
	default:
		return fmt.Errorf("unknown update-check channel %q (want %s or %s)", channel, ChannelStable, ChannelPrerelease)
	}
}

// ValidateChannel reports whether channel names a supported update channel.
func ValidateChannel(channel string) error { return validateChannel(channel) }

// Supervised reports whether root has the mutable binary slot `self-update`
// requires. Without it `goobers self-update` refuses, so a notice must point
// the operator at `goobers service install` instead.
func Supervised(root string) bool {
	_, err := os.Stat(currentBinary(root, runtime.GOOS))
	return err == nil
}

// WriteCheck caches result under root for `goobers status` to render, so a
// status call never makes a request of its own.
func WriteCheck(root string, result CheckResult) error {
	if result.Schema == "" {
		result.Schema = checkSchema
	}
	if _, err := writeJSONAtomic(checkPath(root), result); err != nil {
		return fmt.Errorf("cache update check: %w", err)
	}
	return nil
}

// ReadCheck returns the cached result. A missing cache returns os.ErrNotExist,
// which callers treat as "no check has run yet" rather than an error.
func ReadCheck(root string) (CheckResult, error) {
	raw, err := os.ReadFile(checkPath(root))
	if err != nil {
		return CheckResult{}, err
	}
	var result CheckResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return CheckResult{}, fmt.Errorf("decode cached update check: %w", err)
	}
	if result.Schema != checkSchema {
		return CheckResult{}, fmt.Errorf("unsupported update check schema %q", result.Schema)
	}
	return result, nil
}

// Notice renders the one-line operator notice for result, or "" when there is
// nothing to say. supervised selects the call to action: `self-update` refuses
// without the supervised binary slot, so an unsupervised instance is pointed at
// `goobers service install` rather than into that error.
func Notice(result CheckResult, supervised bool) string {
	if !result.UpdateAvailable {
		return ""
	}
	action := "run `goobers self-update` to stage it"
	if !supervised {
		action = "not supervised; run `goobers service install` first"
	}
	return fmt.Sprintf("update available: %s (running %s) — %s",
		result.LatestVersion, result.CurrentVersion, action)
}
