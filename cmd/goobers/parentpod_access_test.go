package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/telemetry"
)

type parentAccessRoundTrip func(*http.Request) (*http.Response, error)

func (f parentAccessRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestParentPodAccessUsesExactContractAndRevokes(t *testing.T) {
	run := strings.Repeat("a", 32)
	digest := "sha256:" + strings.Repeat("b", 64)
	var methods []string
	transport := parentAccessRoundTrip(func(r *http.Request) (*http.Response, error) {
		methods = append(methods, r.Method)
		if r.URL.String() != "https://daemon.invalid/api/v1/runs/"+run+"/child-workflow-access" || r.Header.Get("Authorization") != "Bearer parent-only" {
			t.Fatal(r.URL, r.Header)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload) != 1 || payload["contractDigest"] != digest {
			t.Fatal(payload)
		}
		data, _ := json.Marshal(mcpio.ChildWorkflowAccess{Endpoint: "https://daemon.invalid", BearerToken: "goobers-child.exact"})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	})
	client := parentAccessClient{Endpoint: "https://daemon.invalid", Token: "parent-only", ContractDigest: digest, Client: &http.Client{Transport: transport}}
	access, err := client.acquire(t.Context(), run)
	if err != nil || access.BearerToken != "goobers-child.exact" {
		t.Fatal(access, err)
	}
	if err = client.revoke(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "POST,DELETE" {
		t.Fatal(methods)
	}
}

func TestParentPodGrantNeverFollowsRedirectOrLeaksRefusalBody(t *testing.T) {
	count := 0
	client := parentAccessClient{Endpoint: "https://daemon.invalid", Token: "secret", ContractDigest: "sha256:" + strings.Repeat("b", 64), Client: &http.Client{Transport: parentAccessRoundTrip(func(*http.Request) (*http.Response, error) {
		count++
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": []string{"https://other.invalid"}}, Body: io.NopCloser(strings.NewReader("secret"))}, nil
	})}}
	_, err := client.acquire(t.Context(), strings.Repeat("a", 32))
	if err == nil || strings.Contains(err.Error(), "secret") || count != 1 {
		t.Fatal(count, err)
	}
}

func TestIsolatedObservedUsageKeepsAdapterSnapshot(t *testing.T) {
	u := &isolatedObservedUsage{}
	u.report(map[string]float64{telemetry.AttrUsageCostUSD: 1, "authored.other": 100})
	u.report(map[string]float64{telemetry.AttrUsageCostUSD: 2})
	got, reported := u.snapshot()
	if !reported || len(got) != 1 || got[telemetry.AttrUsageCostUSD] != 2 {
		t.Fatal(got)
	}
	surrendered := dispatcher.SurrenderedResult{Result: apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Metrics: map[string]float64{telemetry.AttrUsageCostUSD: 999}}, ObservedUsage: got, ObservedUsageReported: true}
	if err := surrendered.Validate(); err != nil {
		t.Fatal(err)
	}
}
