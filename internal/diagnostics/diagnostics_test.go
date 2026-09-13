package diagnostics

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/journal"
)

func fixedNow() time.Time { return time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC) }

// leakyScrubber is the real pattern net, so the redaction assertions below
// exercise what production actually applies rather than a stand-in.
func realScrubber() Scrubber { return journal.NewPatternScrubber() }

func collectorForTest() Collector {
	return Collector{
		Binary: BinaryInfo{
			Version: "v1.2.3", Commit: "abc123", Date: "2026-09-01",
			OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version(),
		},
		Contract: ContractInfo{
			JournalSchemaVersion: 7,
			DSLVersions:          []string{"2.0", "3.0"},
			StageCommands: []StageCommandInfo{
				{Command: "backlog-query", ResultFile: "claimed-item.json", Capabilities: []string{"github:issues:write"}},
			},
		},
		Scrubber: realScrubber(),
	}
}

// The headline claim: an operator on a machine with no Goobers checkout can
// answer "why did the selector return no-work while eligible-looking PRs
// existed" from the bundle alone.
func TestBundleExplainsANoWorkCycleWithoutASourceCheckout(t *testing.T) {
	c := collectorForTest()
	c.RunDirs = func(string) ([]string, error) { return []string{"run-a"}, nil }
	c.Instance = func(string) (InstanceInfo, []CredentialPresence, error) {
		return InstanceInfo{
			ConfigDigest: "sha256:cafe",
			Gaggles: []GaggleInfo{{
				Name:      "goobers",
				Workflows: []WorkflowInfo{{Name: "merge-review", DSLVersion: "2.0", DefinitionDigest: "sha256:beef"}},
				Goobers:   []string{"reviewer"},
			}},
		}, []CredentialPresence{
			CredentialPresenceFor("github:issues:write", "env", "GH_ISSUES", func(string) (string, bool) { return "", true }),
		}, nil
	}
	c.Daemon = func(string, time.Time) (DaemonInfo, error) {
		return DaemonInfo{Running: true, LockPresent: true, PID: 41, Version: "v1.2.3"}, nil
	}

	bundle, err := c.Collect(Options{Root: "/instances/prod", Now: fixedNow()})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if bundle.Schema != Schema {
		t.Fatalf("schema = %q", bundle.Schema)
	}
	// The four "which generation made this decision" facts.
	if bundle.Binary.Version != "v1.2.3" || bundle.Contract.JournalSchemaVersion != 7 ||
		bundle.Instance.ConfigDigest != "sha256:cafe" || !bundle.Daemon.Running {
		t.Fatalf("bundle = %+v", bundle)
	}
	// The contract surface, so a bundle is read against the right contract
	// rather than against whatever the reader's checkout happens to be at.
	if len(bundle.Contract.StageCommands) != 1 ||
		bundle.Contract.StageCommands[0].Capabilities[0] != "github:issues:write" {
		t.Fatalf("contract = %+v", bundle.Contract)
	}
	summary := Summary(bundle)
	for _, want := range []string{"goobers v1.2.3", "merge-review", "github:issues:write", "source present"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary is missing %q:\n%s", want, summary)
		}
	}
}

// TestSummaryDaemonDistinguishesManualLockFromCrash is #4833's regression: a
// foreground `goobers run` acquires the same up.lock the daemon does and
// leaves it behind on exit (holderKind "manual") — that must never read as
// "a previous daemon exited without releasing it", the exact false claim
// diagnostics bundle made on the demo onboarding path (init --demo, run
// demo, no daemon ever started). Pins the exact summary sentence for the
// manual-lock, no-lock, and (still-legitimate) daemon-crash cases so the
// distinction cannot silently regress.
func TestSummaryDaemonDistinguishesManualLockFromCrash(t *testing.T) {
	c := collectorForTest()
	c.RunDirs = func(string) ([]string, error) { return nil, nil }
	c.Instance = func(string) (InstanceInfo, []CredentialPresence, error) {
		return InstanceInfo{}, nil, nil
	}

	for _, test := range []struct {
		name   string
		info   DaemonInfo
		want   string
		unwant []string
	}{
		{
			name:   "manual lock is not a crash",
			info:   DaemonInfo{LockPresent: true, LockHolderKind: "manual"},
			want:   "Not running, and no daemon crash indicated: the lock file was left by a foreground `goobers run`, not a daemon.",
			unwant: []string{"previous daemon exited"},
		},
		{
			name:   "no lock at all",
			info:   DaemonInfo{},
			want:   "Not running, and no lock file is present.",
			unwant: []string{"previous daemon exited", "foreground"},
		},
		{
			name:   "daemon-held lock with nothing running is still a crash claim",
			info:   DaemonInfo{LockPresent: true, LockHolderKind: "daemon"},
			want:   "Not running, but a lock file is present — a previous daemon exited without releasing it.",
			unwant: []string{"foreground"},
		},
		{
			name:   "unknown holder kind (legacy lock file) stays the conservative crash claim",
			info:   DaemonInfo{LockPresent: true},
			want:   "Not running, but a lock file is present — a previous daemon exited without releasing it.",
			unwant: []string{"foreground"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			c.Daemon = func(string, time.Time) (DaemonInfo, error) { return test.info, nil }
			bundle, err := c.Collect(Options{Root: "/instances/demo", Now: fixedNow()})
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			summary := Summary(bundle)
			if !strings.Contains(summary, test.want) {
				t.Fatalf("summary = %q, want it to contain %q", summary, test.want)
			}
			for _, bad := range test.unwant {
				if strings.Contains(summary, bad) {
					t.Fatalf("summary = %q, must not contain %q", summary, bad)
				}
			}
		})
	}
}

// A support bundle collected during an incident must be produced from whatever
// IS readable, and must SAY what it could not read: an omission stated is not
// an omission found.
func TestUnreadableSourcesBecomeNotesRatherThanFailures(t *testing.T) {
	c := collectorForTest()
	c.Instance = func(string) (InstanceInfo, []CredentialPresence, error) {
		return InstanceInfo{}, nil, os.ErrPermission
	}
	c.Daemon = func(string, time.Time) (DaemonInfo, error) { return DaemonInfo{}, os.ErrPermission }
	c.RunDirs = func(string) ([]string, error) { return nil, os.ErrPermission }

	bundle, err := c.Collect(Options{Root: "/instances/prod", Now: fixedNow()})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(bundle.Notes) != 3 {
		t.Fatalf("notes = %v, want one per unreadable source", bundle.Notes)
	}
	if !strings.Contains(Summary(bundle), "## Not collected") {
		t.Error("summary does not tell the reader what was omitted")
	}
}

// An explicitly named run that does not exist is the one collection failure
// that must be hard: silently returning an empty bundle would answer the
// operator's question with the wrong evidence.
func TestNamedRunThatDoesNotExistIsAnError(t *testing.T) {
	c := collectorForTest()
	c.RunDirs = func(string) ([]string, error) { return nil, nil }
	if _, err := c.Collect(Options{Root: "/instances/prod", Now: fixedNow(), RunID: "missing"}); err == nil {
		t.Fatal("err = nil, want a refusal naming the run")
	}
}

// Redaction is two mechanisms, and this asserts both. STRUCTURAL: a credential
// contributes a name and a source kind, never a value — the collector is never
// even handed one. TEXTUAL: a token pasted into an error message is redacted on
// the way out.
func TestBundleRedactsSecretsInSurvivingText(t *testing.T) {
	const leaked = "ghp_0123456789abcdefghijklmnopqrstuvwxyzA"
	c := collectorForTest()
	c.RunDirs = func(string) ([]string, error) { return nil, nil }
	c.Instance = func(string) (InstanceInfo, []CredentialPresence, error) {
		return InstanceInfo{}, []CredentialPresence{
			// The source NAME is all the projection carries. There is no field
			// on CredentialPresence that could hold a value.
			CredentialPresenceFor("repo:push", "env", "GOOBERS_TOKEN", func(string) (string, bool) { return leaked, true }),
		}, nil
	}
	c.Daemon = func(string, time.Time) (DaemonInfo, error) {
		return DaemonInfo{}, os.ErrPermission
	}

	bundle, err := c.Collect(Options{Root: "/instances/prod", Now: fixedNow()})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(leaked)) {
		t.Fatalf("bundle carries a credential value:\n%s", encoded)
	}
	// Presence is recorded; the value that established it is not.
	if len(bundle.Credentials) != 1 || bundle.Credentials[0].Present == nil || !*bundle.Credentials[0].Present {
		t.Fatalf("credentials = %+v, want presence recorded", bundle.Credentials)
	}

	// The textual backstop, on content the projection legitimately keeps.
	c.RunDirs = nil
	c.Instance = func(string) (InstanceInfo, []CredentialPresence, error) {
		return InstanceInfo{}, nil, nil
	}
	c.Daemon = func(string, time.Time) (DaemonInfo, error) {
		return DaemonInfo{}, os.ErrPermission
	}
	scrubbed := c.scrub(Bundle{
		Notes: []string{"boom: " + leaked},
		Runs: []RunInfo{{
			RunID:         "run-a",
			DecisiveError: &ErrorInfo{Message: "auth failed for " + leaked},
			Stages:        []StageInfo{{Error: &ErrorInfo{Message: "stage saw " + leaked}}},
			Decisions:     []Decision{{Reason: "excluded because " + leaked}},
		}},
		Credentials: []CredentialPresence{{SourceName: leaked}},
	})
	encoded, err = json.Marshal(scrubbed)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(leaked)) {
		t.Fatalf("scrubbed bundle still carries the token:\n%s", encoded)
	}
}

// This fixture is built from Bundle's type rather than a second field list.
// Adding a string anywhere in the schema therefore adds a redaction assertion
// automatically, which is the future-proof property the reflective production
// walk exists to provide.
func TestBundleRedactsEveryStringField(t *testing.T) {
	const leaked = "ghp_0123456789abcdefghijklmnopqrstuvwxyzA"
	value, stringCount := bundleValueWithEveryString(t, reflect.TypeOf(Bundle{}), leaked, 0)
	if stringCount == 0 {
		t.Fatal("Bundle unexpectedly has no string fields")
	}

	bundle := value.Interface().(Bundle)
	original, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	scrubbed := (Collector{Scrubber: realScrubber()}).scrub(bundle)
	assertNoBundleStringContains(t, reflect.ValueOf(scrubbed), leaked, "Bundle")
	after, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatalf("scrubbing mutated its input graph:\n got %s\nwant %s", after, original)
	}

	encoded, err := json.Marshal(scrubbed)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(leaked)) {
		t.Fatalf("scrubbed bundle still contains token: %s", encoded)
	}
}

func bundleValueWithEveryString(t *testing.T, typ reflect.Type, token string, depth int) (reflect.Value, int) {
	t.Helper()
	if depth > 24 {
		t.Fatalf("Bundle contains a recursive type at %s", typ)
	}
	switch typ.Kind() {
	case reflect.String:
		value := reflect.New(typ).Elem()
		value.SetString(token)
		return value, 1
	case reflect.Pointer:
		elem, count := bundleValueWithEveryString(t, typ.Elem(), token, depth+1)
		value := reflect.New(typ.Elem())
		value.Elem().Set(elem)
		return value, count
	case reflect.Struct:
		value := reflect.New(typ).Elem()
		count := 0
		for i := 0; i < typ.NumField(); i++ {
			if typ.Field(i).PkgPath != "" {
				continue
			}
			field, fieldCount := bundleValueWithEveryString(t, typ.Field(i).Type, token, depth+1)
			value.Field(i).Set(field)
			count += fieldCount
		}
		return value, count
	case reflect.Slice:
		elem, count := bundleValueWithEveryString(t, typ.Elem(), token, depth+1)
		value := reflect.MakeSlice(typ, 1, 1)
		value.Index(0).Set(elem)
		return value, count
	case reflect.Array:
		value := reflect.New(typ).Elem()
		count := 0
		for i := 0; i < typ.Len(); i++ {
			elem, elemCount := bundleValueWithEveryString(t, typ.Elem(), token, depth+1)
			value.Index(i).Set(elem)
			count += elemCount
		}
		return value, count
	case reflect.Map:
		key, keyCount := bundleValueWithEveryString(t, typ.Key(), token, depth+1)
		elem, elemCount := bundleValueWithEveryString(t, typ.Elem(), token, depth+1)
		value := reflect.MakeMapWithSize(typ, 1)
		value.SetMapIndex(key, elem)
		return value, keyCount + elemCount
	case reflect.Interface:
		t.Fatalf("Bundle gained interface field %s; give the exhaustive fixture a concrete value", typ)
	}
	return reflect.Zero(typ), 0
}

func assertNoBundleStringContains(t *testing.T, value reflect.Value, token, path string) {
	t.Helper()
	if !value.IsValid() {
		return
	}
	switch value.Kind() {
	case reflect.String:
		if strings.Contains(value.String(), token) {
			t.Errorf("%s still contains the token", path)
		}
	case reflect.Pointer, reflect.Interface:
		if !value.IsNil() {
			assertNoBundleStringContains(t, value.Elem(), token, path)
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).PkgPath == "" {
				assertNoBundleStringContains(t, value.Field(i), token, path+"."+value.Type().Field(i).Name)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			assertNoBundleStringContains(t, value.Index(i), token, path)
		}
	case reflect.Map:
		iter := value.MapRange()
		for iter.Next() {
			assertNoBundleStringContains(t, iter.Key(), token, path)
			assertNoBundleStringContains(t, iter.Value(), token, path)
		}
	}
}

// Determinism is a contract, not a nicety: two collections of the same state
// must be byte-comparable, or "reproduce it on the support machine" is not a
// testable claim. It must hold on every platform — an archive assembled with a
// host path separator or a local timezone would not.
func TestArchiveIsByteIdenticalForTheSameState(t *testing.T) {
	bundle := Bundle{
		Schema:      Schema,
		GeneratedAt: fixedNow().Format(time.RFC3339),
		Binary:      BinaryInfo{Version: "v1.2.3", OS: runtime.GOOS, Arch: runtime.GOARCH},
		Instance:    InstanceInfo{Root: "/instances/prod"},
		Runs:        []RunInfo{{RunID: "run-a", Workflow: "merge-review"}},
	}
	var first, second bytes.Buffer
	if err := WriteArchive(&first, bundle); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	if err := WriteArchive(&second, bundle); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("two archives of the same bundle differ; the bundle is not reproducible")
	}

	entries := readArchive(t, first.Bytes())
	if len(entries) != 2 {
		t.Fatalf("archive entries = %d, want the JSON and the summary", len(entries))
	}
	for _, name := range []string{FileJSON, FileSummary} {
		if _, ok := entries[name]; !ok {
			t.Fatalf("archive is missing %s", name)
		}
	}
	// Slash-separated names on every platform, or the archive is not portable.
	for name := range entries {
		if strings.ContainsRune(name, filepath.Separator) && filepath.Separator != '/' {
			t.Fatalf("archive entry %q carries a host path separator", name)
		}
	}
	var decoded Bundle
	if err := json.Unmarshal(entries[FileJSON], &decoded); err != nil {
		t.Fatalf("archive JSON does not decode: %v", err)
	}
	if decoded.Runs[0].RunID != "run-a" {
		t.Fatalf("decoded = %+v", decoded)
	}
}

// The archive must not embed the collecting host's clock or filenames, which
// is what makes two collections comparable at all.
func TestArchiveHeadersCarryNoHostState(t *testing.T) {
	var buf bytes.Buffer
	bundle := Bundle{Schema: Schema, GeneratedAt: fixedNow().Format(time.RFC3339)}
	if err := WriteArchive(&buf, bundle); err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	gz, err := gzip.NewReader(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if gz.Name != "" || !gz.ModTime.IsZero() {
		t.Fatalf("gzip header carries host state: name=%q modTime=%v", gz.Name, gz.ModTime)
	}
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Uid != 0 || header.Gid != 0 || header.Uname != "" || header.Gname != "" {
			t.Fatalf("%s header carries host ownership: %+v", header.Name, header)
		}
		if !header.ModTime.Equal(fixedNow()) {
			t.Fatalf("%s modTime = %v, want the bundle's own GeneratedAt", header.Name, header.ModTime)
		}
	}
}

// Presence must be answerable without resolving anything. For sources whose
// presence cannot be established that way, the bundle says "not determinable"
// rather than guessing — asking the resolver would materialize the secret this
// bundle exists to never touch.
func TestCredentialPresenceNeverResolvesASecret(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "token")
	if err := os.WriteFile(existing, []byte("ghp_secret"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := CredentialPresenceFor("repo:push", "env", "SET_VAR", func(string) (string, bool) { return "v", true })
	if env.Present == nil || !*env.Present {
		t.Fatalf("env presence = %+v", env)
	}
	missing := CredentialPresenceFor("repo:push", "env", "UNSET_VAR", func(string) (string, bool) { return "", false })
	if missing.Present == nil || *missing.Present {
		t.Fatalf("missing env presence = %+v", missing)
	}
	file := CredentialPresenceFor("repo:push", "file", existing, nil)
	if file.Present == nil || !*file.Present {
		t.Fatalf("file presence = %+v", file)
	}
	if data, err := json.Marshal(file); err != nil || bytes.Contains(data, []byte("ghp_secret")) {
		t.Fatalf("file presence leaked its contents: %s (%v)", data, err)
	}
	for _, kind := range []string{"keychain", "store", "githubCLI"} {
		record := CredentialPresenceFor("agent:model", kind, "name", nil)
		if record.Present != nil {
			t.Fatalf("%s presence = %v, want not determinable without resolving it", kind, *record.Present)
		}
	}
}

func readArchive(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string][]byte{}
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = body
	}
	return entries
}
