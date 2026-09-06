package instance

import (
	"reflect"
	"sort"
)

// tokenRefType is the one struct shape a guarded path can live in. Comparing
// against it by type — rather than by field name — is what lets the walk below
// find a file-backed ref wherever a future Config field puts one.
var tokenRefType = reflect.TypeOf(TokenRef{})

// maxGuardedWalkDepth bounds the reflective descent. Config is a finite tree of
// value types with no recursive shape, so this is never reached in practice; it
// exists so a future self-referential field turns into a truncated result
// rather than a daemon that never finishes loading its own config.
const maxGuardedWalkDepth = 24

// GuardedCredentialPaths enumerates every on-disk path a loaded Config
// references via a file-backed TokenRef — a repo's PAT or GitHub App private
// key, the workflow source's token/private key, the daemon identity's
// token/private key, a credential grant's token file, the GitHub webhook
// secret, an OTLP collector's auth header values, and anything a future
// TokenRef-carrying field adds (#4273).
//
// It is the input to the deterministic executor's narrow stage-command
// refusal (internal/executor.ShellExecutor.GuardedCredentialPaths): a set of
// paths a stage's command, script, or environment must never reference,
// derived from config rather than hardcoded.
//
// The enumeration is a REFLECTIVE walk of the Config graph collecting every
// TokenRef.File it reaches, not a hand-written list of the fields that carry
// one today. That difference is the point: a hand-written list is only correct
// until the next TokenRef field lands, and the failure mode of missing one is
// silent — the new credential file simply is not guarded, with nothing in the
// build to say so. The first version of this function was such a list, and it
// had already missed two live consumers (WebhookConfig.Secret and
// OTLPConfig.Headers) by the time it was written.
//
// This does NOT enumerate every secret an instance holds — only file paths THIS
// config names. An env-, keychain-, or store-backed ref carries no path of its
// own, and a secretStores entry authenticates through ambient identity (Azure
// workload identity, `az cli`) with no file to guard. Those are outside what a
// path-based tripwire can express, by construction.
//
// The result is deduplicated and sorted, so a caller comparing two loads of the
// same config sees the same slice.
func GuardedCredentialPaths(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	seen := make(map[string]struct{})
	collectGuardedPaths(reflect.ValueOf(cfg), seen, 0)
	if len(seen) == 0 {
		return nil
	}
	paths := make([]string, 0, len(seen))
	for path := range seen {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

// collectGuardedPaths descends v, adding every non-empty TokenRef.File it
// reaches to seen. Unexported fields are skipped: Config's credential-carrying
// surface is its exported, YAML/JSON-decoded shape, and reading an unexported
// field reflectively is both unnecessary here and a panic risk.
func collectGuardedPaths(v reflect.Value, seen map[string]struct{}, depth int) {
	if depth > maxGuardedWalkDepth || !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return
		}
		collectGuardedPaths(v.Elem(), seen, depth+1)
	case reflect.Struct:
		if v.Type() == tokenRefType {
			if file := v.FieldByName("File").String(); file != "" {
				seen[file] = struct{}{}
			}
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).PkgPath != "" {
				continue
			}
			collectGuardedPaths(v.Field(i), seen, depth+1)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			collectGuardedPaths(v.Index(i), seen, depth+1)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			collectGuardedPaths(v.MapIndex(key), seen, depth+1)
		}
	}
}
