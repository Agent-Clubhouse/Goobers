package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

func TestAccountPathScrubberText(t *testing.T) {
	scrub := newAccountPathScrubber("/srv/svc-alice")
	for _, test := range []struct{ in, want string }{
		{
			`fork/exec "/Users/alice/go/bin/goobers" --mcp-config /Users/alice/.goobers/mcp.json: exit status 1`,
			`fork/exec "<home>/go/bin/goobers" --mcp-config <home>/.goobers/mcp.json: exit status 1`,
		},
		{`remove /home/alice/worktrees/run-1: permission denied`, `remove <home>/worktrees/run-1: permission denied`},
		{`/Users/alice`, `<home>`},
		{`root=/var/home/alice/x`, `root=<home>/x`},
		{`PATH=/usr/bin:/home/alice/bin`, `PATH=/usr/bin:<home>/bin`},
		{`open file:///Users/alice/x.json`, `open file://<home>/x.json`},
		{`cd /mnt/c/Users/alice/src`, `cd <home>/src`},
		{`["/home/alice/a","/home/bob/b"]`, `["<home>/a","<home>/b"]`},
		// Paths glued to a compiler/linker flag, a response file, or a shell
		// operator.
		{`clang -I/Users/alice/include -L/home/alice/lib`, `clang -I<home>/include -L<home>/lib`},
		{`ld: warning: directory not found for option '-L/Users/alice/lib'`, `ld: warning: directory not found for option '-L<home>/lib'`},
		{`swiftc @/Users/alice/args.rsp`, `swiftc @<home>/args.rsp`},
		{`cp x ~/a&&/home/alice/x`, `cp x ~/a&&<home>/x`},
		{`cl -IC:\Users\alice\inc`, `cl -I<home>\inc`},
		// Escaped newlines and tabs in a truncated JSON message (stack traces).
		{`{"error":"failed:\n/Users/alice/x\tand\t/home/bob/y`, `{"error":"failed:\n<home>/x\tand\t<home>/y`},
		{`{"error":"panic\n\tC:\\Users\\alice\\go\\x.go:12`, `{"error":"panic\n\t<home>\\go\\x.go:12`},
		{`C:\Users\alice\AppData\Local\goobers.exe --mcp-config C:\Users\alice\.goobers\mcp.json`, `<home>\AppData\Local\goobers.exe --mcp-config <home>\.goobers\mcp.json`},
		{`c:/users/Alice/src/x`, `<home>/src/x`},
		{`--config=C:\Users\alice\x`, `--config=<home>\x`},
		{`C:\Users\Jane Doe\AppData\x failed`, `<home>\AppData\x failed`},
		{`C:\Users\John (Contoso)\x`, `<home>\x`},
		{`open C:\Users\alice: access denied`, `open <home>: access denied`},
		{`C:\Users\alice has no access to \\server\share`, `<home> has no access to \\server\share`},
		{`\\?\C:\Users\alice\x`, `<home>\x`},
		{`\\fileserver\c$\Users\alice\x`, `<home>\x`},
		// JSON-escaped text (for example a message truncated mid-document).
		{`{"error":"C:\\Users\\alice\\x","path":"/home/alice/y"`, `{"error":"<home>\\x","path":"<home>/y"`},
		{`{"error":"C:\\Users\\Jane Doe\\x"`, `{"error":"<home>\\x"`},
		// The daemon account's own home outside the conventional roots.
		{`/srv/svc-alice/instance/up.lock`, `<home>/instance/up.lock`},
		{`/srv/svc-alice`, `<home>`},
		{`stale lock in /srv/svc-alice.`, `stale lock in <home>.`},
		{`/srv/svc-alice2/instance`, `/srv/svc-alice2/instance`},
		{`/srv/svc-alice.bak/instance`, `/srv/svc-alice.bak/instance`},
		{`/data/srv/svc-alice/x`, `/data/srv/svc-alice/x`},
		// Not account paths (relative or URL segments): byte-for-byte unchanged.
		{`GET https://api.github.com/users/octocat: 404 Not Found`, `GET https://api.github.com/users/octocat: 404 Not Found`},
		{`GET https://graph.microsoft.com/v1.0/Users/abc: 404`, `GET https://graph.microsoft.com/v1.0/Users/abc: 404`},
		{`GET https://scim.example.com/scim/v2/Users/2819c223: 404`, `GET https://scim.example.com/scim/v2/Users/2819c223: 404`},
		{`see https://example.com/home/docs`, `see https://example.com/home/docs`},
		{`lint failed: web/src/pages/home/index.tsx:12`, `lint failed: web/src/pages/home/index.tsx:12`},
		{`src/Features/Users/UserService.cs(12,5): error CS0103`, `src/Features/Users/UserService.cs(12,5): error CS0103`},
		{`src\Features\Users\UserService.cs(12,5): error`, `src\Features\Users\UserService.cs(12,5): error`},
		{`see pre-/home/alice and a-b/Users/c`, `see pre-/home/alice and a-b/Users/c`},
		{`/var/lib/goobers/runs/x and /tmp/goobers-1`, `/var/lib/goobers/runs/x and /tmp/goobers-1`},
		{`stage failed: exit status 1`, `stage failed: exit status 1`},
	} {
		if got := scrub.text(test.in); got != test.want {
			t.Errorf("text(%q)\n got %q\nwant %q", test.in, got, test.want)
		}
	}
}

func TestAccountPathScrubberDaemonHome(t *testing.T) {
	for _, test := range []struct{ home, in, want string }{
		{`D:\Profiles\svc-alice\`, `D:\Profiles\svc-alice\instance\up.lock`, `<home>\instance\up.lock`},
		{`D:\Profiles\svc-alice`, `d:/profiles/SVC-ALICE/instance`, `<home>/instance`},
		{`D:\Profiles\svc-alice`, `"D:\\Profiles\\svc-alice\\x"`, `"<home>\\x"`},
		{`D:\Profiles\svc-alice`, `D:\Profiles\svc-alice2\x`, `D:\Profiles\svc-alice2\x`},
		// A rune, not byte, boundary: é is two bytes and Go's \b is ASCII-only.
		{"/srv/josé", "/srv/josé/x and /srv/joséphine/x", "<home>/x and /srv/joséphine/x"},
		// POSIX homes are case-sensitive and anchored at a path start.
		{"/root", "/root/x, /var/lib/root/x and /Root/y", "<home>/x, /var/lib/root/x and /Root/y"},
		{"/var/lib/goobers", "/var/lib/goobers/runs and /var/lib/goobers-old and x/var/lib/goobers/y", "<home>/runs and /var/lib/goobers-old and x/var/lib/goobers/y"},
		// An empty or root home names no account.
		{"", "/srv/x", "/srv/x"},
		{"/", "/srv/x", "/srv/x"},
		{`\`, "/srv/x", "/srv/x"},
	} {
		if got := newAccountPathScrubber(test.home).text(test.in); got != test.want {
			t.Errorf("home %q: text(%q)\n got %q\nwant %q", test.home, test.in, got, test.want)
		}
	}
}

func TestAccountPathScrubberCandidateCatchesEscapedForms(t *testing.T) {
	scrub := newAccountPathScrubber("/srv/svc-alice")
	for _, line := range []string{
		`{"message":"{\"p\":\"\\/Users\\/alice\\/x\"}"}`,
		`{"message":"{\"p\":\"\\u002fUsers\\u002falice\"}"}`,
		`{"message":"/srv/svc-alice/x"}`,
		`{"message":"{\"instanceRoot\":\"/opt/x\"}"}`,
	} {
		if !scrub.candidate([]byte(line)) {
			t.Errorf("prefilter missed %s", line)
		}
		got, err := azureAccountPathPayload([]byte(line), scrub)
		if err != nil || strings.Contains(string(got), "alice") || strings.Contains(string(got), "instanceRoot") {
			t.Errorf("escaped form survived: %s, %v", got, err)
		}
	}
	if scrub.candidate([]byte(`{"message":"stage failed: exit status 1"}`)) {
		t.Error("prefilter matched a line with no candidate")
	}
}

func TestAccountPathPlaceholderMatchesProfileContract(t *testing.T) {
	contract := instance.TelemetryCollectionContract()
	if contract.ConsentGatedPathPlaceholder != accountPathPlaceholder {
		t.Fatalf("contract placeholder %q, exporter uses %q", contract.ConsentGatedPathPlaceholder, accountPathPlaceholder)
	}
	if !slices.Contains(contract.ConsentGatedIdentity, accountRootField) {
		t.Fatalf("contract does not list %q as consent-gated", accountRootField)
	}
}

func TestAzureAccountPathPayloadEdges(t *testing.T) {
	scrub := newAccountPathScrubber("")
	// No candidate, and a candidate that needs no change, stay byte-identical
	// (including the encoder's escaping and key order).
	for _, line := range []string{
		`{"data":{"baseData":{"message":"stage failed: exit status 1","properties":{"z":"1","a":"\u003c"}}}}`,
		`{"data":{"baseData":{"message":"the users list is empty","properties":{"z":"1","a":"\u003c"}}}}`,
	} {
		raw := []byte(line + "\n")
		if got, err := azureAccountPathPayload(raw, scrub); err != nil || !bytes.Equal(got, raw) {
			t.Fatalf("unchanged line rewritten: %s, %v", got, err)
		}
	}
	if _, err := azureAccountPathPayload([]byte(`{"message":"/Users/alice/x"`), scrub); err == nil {
		t.Fatal("malformed candidate bypassed the fail-closed HTTP boundary")
	}

	// An already-encoded (for example replayed) journal message: escaped
	// Windows paths, case-variant keys and large integers inside the nested
	// document are all handled structurally.
	body := `{"type":"daemon.started","seq":9007199254740993,"runner":{"InstanceRoot":"D:\\custom\\root","pid":4242,"error":"C:\\Users\\alice\\x"}}`
	line, err := json.Marshal(map[string]any{"data": map[string]any{"baseData": map[string]any{"message": body, "properties": map[string]string{"goobers.telemetry.record_id": "stable"}}}})
	if err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(line)
	got, err := azureAccountPathPayload(line, scrub)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(line, original) {
		t.Fatal("projection modified the spool's source bytes")
	}
	message := azureEnvelopeMessages(t, string(got))[0]
	if strings.Contains(message, "alice") || strings.Contains(strings.ToLower(message), "instanceroot") || strings.Contains(message, "custom") {
		t.Fatalf("account path survived: %s", message)
	}
	if !strings.Contains(message, `9007199254740993`) || !strings.Contains(message, `"error":"<home>\\x"`) || azureRecordID(string(got)) != "stable" {
		t.Fatalf("projection lost data, precision or correlation: %s", message)
	}

	// Object keys are scrubbed too, in nested documents and in the envelope;
	// a document whose values need no change still has its keys checked.
	keyed := `{"data":{"baseData":{"message":"{\"type\":\"x\",\"paths\":{\"/Users/alice/wt\":1}}","properties":{"C:\\Users\\alice\\k":"v"}}}}`
	got, err = azureAccountPathPayload([]byte(keyed), scrub)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "alice") || !strings.Contains(azureEnvelopeMessages(t, string(got))[0], `"<home>/wt":1`) {
		t.Fatalf("account path in a key survived: %s", got)
	}
}

func TestAzureMonitorJournalWithholdsAccountPathsWithoutIdentityConsent(t *testing.T) {
	const (
		started = `{"schema":"goobers.dev/journal/event/v1","seq":3,"type":"daemon.started","runner":{"instanceRoot":"/Users/alice/goobers/instance","pid":4242,"startedAt":"2026-10-01T00:00:00Z","version":"v0.5.0"}}`
		failed  = `{"schema":"goobers.dev/journal/event/v1","seq":4,"type":"stage.failed","error":"agent exited 1: /Users/alice/go/bin/claude -p --mcp-config /Users/alice/.goobers/mcp.json; C:\\Users\\alice\\AppData\\x"}`
	)
	for _, consent := range []bool{false, true} {
		t.Run("identity="+strconv.FormatBool(consent), func(t *testing.T) {
			payloads, server := azureMonitorTestServer(t)
			defer server.Close()
			client, err := New(context.Background(), Config{
				ServiceName: "goobers", ServiceVersion: "v0.5.0-test",
				AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL,
				AzureMonitorHTTPClient:       server.Client(), AzureMonitorJournalLogs: true, AzureMonitorHostIdentity: consent,
				JournalLogs: true, JournalLogsOnly: true, Scrubber: journal.NewPatternScrubber(),
			})
			if err != nil {
				t.Fatal(err)
			}
			bodies := [][]byte{[]byte(started), []byte(failed)}
			for i, body := range bodies {
				client.Commit(journal.CommittedEvent{Kind: "scheduler", JournalID: "scheduler-id", InstanceID: "durable-instance", Seq: uint64(i + 3), Time: time.Now(), Body: body})
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := client.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if string(bodies[0]) != started || string(bodies[1]) != failed {
				t.Fatal("export changed the authoritative journal bytes")
			}
			messages := drainAzureMessages(t, payloads, 2)
			if consent {
				// The diagnostic profile's explicit consent exports bodies unchanged.
				if !slices.Contains(messages, started) || !slices.Contains(messages, failed) {
					t.Fatalf("diagnostic profile changed journal bodies: %q", messages)
				}
				return
			}
			for _, message := range messages {
				if strings.Contains(message, "alice") || strings.Contains(message, "instanceRoot") {
					t.Fatalf("account path exported without identity consent: %s", message)
				}
			}
			var lifecycle struct {
				Seq    int
				Type   string
				Runner map[string]any
			}
			if err := json.Unmarshal([]byte(messages[0]), &lifecycle); err != nil {
				t.Fatal(err)
			}
			if lifecycle.Type != "daemon.started" || lifecycle.Seq != 3 || lifecycle.Runner["pid"] != float64(4242) || lifecycle.Runner["version"] != "v0.5.0" {
				t.Fatalf("lifecycle projection lost operational fields: %s", messages[0])
			}
			var failure struct{ Error string }
			if err := json.Unmarshal([]byte(messages[1]), &failure); err != nil {
				t.Fatal(err)
			}
			const want = `agent exited 1: <home>/go/bin/claude -p --mcp-config <home>/.goobers/mcp.json; <home>\AppData\x`
			if failure.Error != want {
				t.Fatalf("error text = %q, want %q", failure.Error, want)
			}
		})
	}
}

func TestAzureMonitorSpanWithholdsAccountPathsWithoutIdentityConsent(t *testing.T) {
	const failure = `harness: exec /home/alice/.local/bin/claude --mcp-config /home/alice/.goobers/mcp.json: exit status 2`
	for _, consent := range []bool{false, true} {
		t.Run("identity="+strconv.FormatBool(consent), func(t *testing.T) {
			payloads, server := azureMonitorTestServer(t)
			defer server.Close()
			client, err := New(context.Background(), Config{
				ServiceName: "goobers", ServiceVersion: "v0.5.0-test",
				AzureMonitorConnectionString: "InstrumentationKey=00000000-0000-0000-0000-000000000000;IngestionEndpoint=" + server.URL,
				AzureMonitorHTTPClient:       server.Client(), AzureMonitorTraces: true, AzureMonitorHostIdentity: consent,
				Scrubber: journal.NewPatternScrubber(),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, span, err := client.StartRun(context.Background(), RunAttributes{Gaggle: "tenant-gaggle", WorkflowID: "poll", RunID: "0af7651916cd43dd8448eb211c80319c"})
			if err != nil {
				t.Fatal(err)
			}
			span.Fail(errors.New(failure))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := client.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Data struct {
					BaseData struct{ Properties map[string]string }
				}
			}
			if err := json.Unmarshal([]byte(waitAzureMonitorPayload(t, payloads)), &envelope); err != nil {
				t.Fatal(err)
			}
			got := envelope.Data.BaseData.Properties["otel.status_description"]
			want := failure
			if !consent {
				want = `harness: exec <home>/.local/bin/claude --mcp-config <home>/.goobers/mcp.json: exit status 2`
			}
			if got != want {
				t.Fatalf("span status = %q, want %q", got, want)
			}
			if envelope.Data.BaseData.Properties["goobers.run.id"] != "0af7651916cd43dd8448eb211c80319c" {
				t.Fatal("projection lost span correlation")
			}
		})
	}
}

// drainAzureMessages collects the journal messages of want records, which the
// exporter may deliver in one or several requests.
func drainAzureMessages(t *testing.T, payloads <-chan string, want int) []string {
	t.Helper()
	var messages []string
	for len(messages) < want {
		messages = append(messages, azureEnvelopeMessages(t, waitAzureMonitorPayload(t, payloads))...)
	}
	slices.SortFunc(messages, func(a, b string) int { return strings.Compare(azureMessageSeq(a), azureMessageSeq(b)) })
	return messages
}

func azureMessageSeq(message string) string {
	var record struct{ Seq json.Number }
	_ = json.Unmarshal([]byte(message), &record)
	return record.Seq.String()
}

func azureEnvelopeMessages(t *testing.T, payload string) []string {
	t.Helper()
	var messages []string
	for _, line := range strings.Split(strings.TrimSpace(payload), "\n") {
		var envelope struct {
			Data struct {
				BaseData struct{ Message string }
			}
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatalf("decode envelope %q: %v", line, err)
		}
		messages = append(messages, envelope.Data.BaseData.Message)
	}
	return messages
}
