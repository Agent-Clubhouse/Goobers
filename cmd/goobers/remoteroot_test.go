package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goobers/goobers/internal/apicontract"
)

const remoteRootFixture = `{"instanceRoot":"/remote/root","rootIdentity":{"id":"0123456789abcdef0123456789abcdef"}}`

func serveRemoteRootFixture(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != apicontract.InstancePath {
		return false
	}
	_, _ = fmt.Fprint(w, remoteRootFixture)
	return true
}

func TestRemoteRootRefusesUnverifiableIdentity(t *testing.T) {
	for name, body := range map[string]string{
		"legacy":              `{"instanceRoot":"/remote/root"}`,
		"malformed":           `null`,
		"bad id":              strings.Replace(remoteRootFixture, "0123456789abcdef0123456789abcdef", "invalid", 1),
		"historical":          strings.Replace(remoteRootFixture, `"id":`, `"decommissionReason":"migrated","id":`, 1),
		"lifecycle unknown":   strings.Replace(remoteRootFixture, `"id":`, `"lifecycleProblem":"unreadable","id":`, 1),
		"trailing":            remoteRootFixture + `{}`,
		"oversized":           remoteRootFixture + strings.Repeat(" ", maxRemoteTriggerResponseBody),
		"duplicate id":        strings.Replace(remoteRootFixture, `"id":`, `"id":"other","id":`, 1),
		"duplicate lifecycle": strings.Replace(remoteRootFixture, `"id":`, `"lifecycleProblem":"broken","lifecycleProblem":"","id":`, 1),
		"case alias":          strings.Replace(remoteRootFixture, `"rootIdentity":`, `"RootIdentity":{},"rootIdentity":`, 1),
		"case id":             strings.Replace(remoteRootFixture, `"id":`, `"ID":`, 1),
		"invalid utf8":        strings.Replace(remoteRootFixture, "/remote/root", "/remote/"+string([]byte{0xff}), 1),
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("mutation sent without valid identity")
				}
				_, _ = fmt.Fprint(w, body)
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			if code := runRemoteTrigger(context.Background(), server.URL, runTarget{Workflow: "test"}, "test", true, &stdout, &stderr); code != 2 {
				t.Fatalf("invalid target accepted: %d %s", code, stderr.String())
			}
		})
	}
}

func TestRemoteRootAuthenticatedDisplayBeforeMutation(t *testing.T) {
	t.Setenv("GOOBERS_API_TOKEN", "test-token")
	var stdout, stderr bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing authorization")
		}
		if serveRemoteRootFixture(w, r) {
			return
		}
		if !strings.Contains(stderr.String(), `Remote instance root: "/remote/root"; instance ID: "0123456789abcdef0123456789abcdef"`) {
			t.Error("mutation preceded identity display")
		}
		_, _ = fmt.Fprint(w, `{"runId":"test"}`)
	}))
	defer server.Close()
	if code := runRemoteTrigger(context.Background(), server.URL, runTarget{Workflow: "test"}, "test", true, &stdout, &stderr); code != 0 {
		t.Fatalf("trigger failed: %d %s", code, stderr.String())
	}
	if err := prepareRemoteRoot(context.Background(), server.URL, brokenRootBannerWriter{}); err == nil {
		t.Fatal("broken identity display accepted")
	}
}

func TestRemoteRootRejectsRedirectAtEitherStep(t *testing.T) {
	for _, redirectIdentity := range []bool{true, false} {
		t.Run(fmt.Sprintf("identity=%t", redirectIdentity), func(t *testing.T) {
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Error("followed redirect to an unidentified daemon")
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !redirectIdentity && serveRemoteRootFixture(w, r) {
					return
				}
				http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
			}))
			defer server.Close()
			var stdout, stderr bytes.Buffer
			if code := runRemoteTrigger(context.Background(), server.URL, runTarget{Workflow: "test"}, "test", true, &stdout, &stderr); code != 2 {
				t.Fatalf("redirect accepted: %d %s", code, stderr.String())
			}
		})
	}
}

func TestRemoteManualCommandsRequireIdentityBeforeMutation(t *testing.T) {
	commands := [][]string{
		{"run", "cancel", "run-1"},
		{"run", "abort", "run-1"},
		{"approve", "--actor=ops", "run-1", "gate"},
		{"override", "--actor=ops", "--rationale=test", "run-1", "gate"},
		{"rerun-stage", "--actor=ops", "--addendum=test", "run-1", "gate"},
		{"escalations", "resolve", "--actor=ops", "--resolution=approve", "--gate=gate", "run-1"},
	}
	for _, args := range commands {
		t.Run(strings.Join(args[:2], "-"), func(t *testing.T) {
			var reads, writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == apicontract.InstancePath {
					reads.Add(1)
					_, _ = fmt.Fprint(w, `{"instanceRoot":"/legacy"}`)
					return
				}
				writes.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			t.Setenv(remoteDaemonAPIEnv, server.URL)
			code, _, stderr := runArgs(t, args...)
			if code != 2 || reads.Load() != 1 || writes.Load() != 0 || !strings.Contains(stderr, "durable root identity") {
				t.Fatalf("identity guard bypassed: code=%d reads=%d writes=%d stderr=%s", code, reads.Load(), writes.Load(), stderr)
			}
		})
	}
}
