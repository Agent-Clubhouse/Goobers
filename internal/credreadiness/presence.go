package credreadiness

import (
	"context"
	"os"

	"github.com/goobers/goobers/internal/credentials"
)

const (
	// StatusPresent establishes source metadata presence, not secret usability.
	StatusPresent Status = "present"
	// StatusAbsent means source metadata establishes absence.
	StatusAbsent Status = "absent"
)

// MetadataProbe may inspect source metadata only. Implementations must never
// resolve, refresh, mint, read, or return a secret. A nil probe leaves sources
// without a local metadata API explicitly unobservable.
type MetadataProbe func(context.Context, SourceKind, string) (bool, error)

// Presence describes a source without constructing a credential resolver (which
// itself resolves GitHub CLI tokens). Values and backend errors never enter the
// result. Presence is not authentication, readability, or target-worker evidence.
func Presence(ctx context.Context, name string, ref credentials.TokenRef, metadata MetadataProbe) Check {
	kind, source := Describe(ref)
	check := Check{Name: name, Kind: kind, Source: source, Category: CategoryAuthentication,
		Status: StatusUnobservable, Detail: "source metadata unavailable without resolving a secret"}
	count := 0
	for _, value := range []string{ref.Env, ref.File, ref.Keychain, ref.Store} {
		if value != "" {
			count++
		}
	}
	if ref.GitHubCLI != nil {
		count++
	}
	invalidCLI := ref.GitHubCLI != nil && (ref.GitHubCLI.Hostname == "" || ref.GitHubCLI.User == "")
	if count != 1 || kind == SourceUnsupported || invalidCLI {
		check.Kind, check.Status, check.Detail = SourceUnsupported, StatusUnsupportedSource, "unsupported or ambiguous credential source; no environment fallback"
		return check
	}
	if ctx.Err() != nil {
		return check
	}
	var present bool
	switch kind {
	case SourceEnv:
		_, present = os.LookupEnv(source)
	case SourceFile:
		info, err := os.Stat(source)
		if err != nil && !os.IsNotExist(err) {
			return check
		}
		if err == nil && !info.Mode().IsRegular() {
			return check
		}
		present = err == nil
	case SourceKeychain, SourceStore, SourceGitHubCLI:
		if metadata == nil {
			return check
		}
		var err error
		present, err = metadata(ctx, kind, source)
		if err != nil {
			return check
		}
	default:
		check.Status = StatusUnsupportedSource
		return check
	}
	check.Status, check.Detail = StatusAbsent, "source metadata absent; no secret read"
	if present {
		check.Status, check.Detail = StatusPresent, "source metadata present; value, readability, expiry and authentication unverified"
	}
	return check
}
