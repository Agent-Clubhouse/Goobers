package main

import (
	"strings"
	"testing"
)

func TestStructuredPartialDeliveryMetadata(t *testing.T) {
	path := writeDocument(t, "# Partial delivery\n\n> Status: approved\n> Owner: @maintainer\n> Area: runtime\n> Tracking: #10\n> Delivered-by: #11\n> Pending-delivery: #12\n> Scope-delta: Remote recovery has not shipped.\n")
	doc, err := parseDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Owner != "@maintainer" || doc.Area != "runtime" || len(doc.Tracking) != 1 || doc.Tracking[0] != "#10" || len(doc.Remaining) != 1 || doc.Remaining[0] != "#12" || doc.ScopeDelta == "" {
		t.Fatalf("metadata lost: %+v", doc)
	}
	if problems := validate([]document{doc}); len(problems) != 0 {
		t.Fatalf("honest partial delivery rejected: %v", problems)
	}
	index := renderIndex([]document{doc})
	for _, want := range []string{"@maintainer / runtime", "#10", "#11", "#12"} {
		if !strings.Contains(index, want) {
			t.Fatalf("index omits %q: %s", want, index)
		}
	}
	for name, mutate := range map[string]func(*document){
		"overstated completion": func(d *document) { d.Status = "implemented" },
		"missing scope delta":   func(d *document) { d.ScopeDelta = "" },
		"contradictory ledger":  func(d *document) { d.Remaining = d.DeliveredBy },
	} {
		t.Run(name, func(t *testing.T) {
			bad := doc
			mutate(&bad)
			if problems := validate([]document{bad}); len(problems) == 0 {
				t.Fatal("contradictory partial delivery accepted")
			}
		})
	}
}

func TestTrackingAndRemainingCannotSilentlyDiscardInvalidReferences(t *testing.T) {
	for _, key := range []string{"Tracking", "Pending-delivery"} {
		for _, value := range []string{"", "some prose"} {
			path := writeDocument(t, "# Design\n> Status: approved\n> "+key+": "+value+"\n")
			if _, err := parseDocument(path); err == nil {
				t.Fatalf("accepted %s: %q", key, value)
			}
		}
	}
}

func TestVerifiedRequiresRevisionAndCalendarDate(t *testing.T) {
	for _, value := range []string{"yesterday", "main (2026-09-07)", "abc1234", "abc1234 (2026-02-30)", "abc1234 (not-a-date)"} {
		if err := validateVerified(value); err == nil {
			t.Fatalf("invalid verification accepted: %q", value)
		}
	}
	for _, value := range []string{"", "09db115bb (2026-09-06)", "abcdef0 (2024-02-29)"} {
		if err := validateVerified(value); err != nil {
			t.Fatalf("valid verification rejected: %q: %v", value, err)
		}
	}
}
