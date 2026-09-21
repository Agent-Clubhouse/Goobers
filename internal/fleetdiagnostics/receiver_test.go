package fleetdiagnostics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	collectorlogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestReferenceFixtureUsesRealOTLPAndTenantQueries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := ReferenceFixture(ctx, &out); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&out)
	seen := 0
	for {
		var result struct {
			Phase, Tenant string
			Reports       []Report
		}
		err := decoder.Decode(&result)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		wantReports := 2
		if result.Phase == "retired_inventory" && result.Tenant == "company-a" {
			wantReports = 1
		}
		if len(result.Reports) != wantReports {
			t.Fatalf("inventory lost: %+v", result)
		}
		for _, r := range result.Reports {
			if r.Organization != result.Tenant || !strings.HasPrefix(r.OwnerRoute, result.Tenant+"/") {
				t.Fatalf("tenant mixed: %+v", result)
			}
			if result.Phase == "two_missed_intervals" && (r.Liveness != "unreachable" || r.Reason != "missing_heartbeat") {
				t.Fatalf("absence not detected: %+v", r)
			}
			if result.Phase == "independent_collector_failure_evidence" && result.Tenant == "company-a" && r.Reason != "collector_unavailable" {
				t.Fatal(r)
			}
			if result.Phase == "initial" && result.Tenant == "company-b" && r.GaggleID == "one" && (r.Features[0].State != "used" || r.Features[0].Count == nil || *r.Features[0].Count != 3) {
				t.Fatalf("observed feature lost: %+v", r)
			}
			if result.Phase == "initial" && result.Tenant == "company-a" && r.GaggleID == "two" && r.Freshness.Status != "outdated" {
				t.Fatal(r)
			}
		}
		seen++
	}
	if seen != 8 {
		t.Fatalf("got %d query snapshots, want8", seen)
	}
}
func TestReceiverAuthenticationClosedContractAndSizeBounds(t *testing.T) {
	now := testTime
	b := backendFixture(t, &now)
	if _, err := NewReceiver(b, nil); err == nil {
		t.Fatal("unauthenticated receiver accepted")
	}
	receiver, _ := NewReceiver(b, func(context.Context) (string, error) { return "tenant-a", nil })
	req := referenceRequest(HeartbeatEvent, heartbeatFields(now))
	attrs := req.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes
	attrs = append(attrs, &commonpb.KeyValue{Key: "prompt", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "private-token-123"}}})
	req.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes = attrs
	response, err := receiver.Export(context.Background(), req)
	if err != nil || response.GetPartialSuccess().GetRejectedLogRecords() != 1 || strings.Contains(response.String(), "private-token") {
		t.Fatalf("private rejection=%v %v", response, err)
	}
	if r := oneReport(t, b); !r.ReceivedAt.IsZero() {
		t.Fatal("rejected private record changed state")
	}
	denied, _ := NewReceiver(b, func(context.Context) (string, error) { return "", errors.New("bad credential") })
	if _, err := denied.Export(context.Background(), req); status.Code(err) != codes.Unauthenticated {
		t.Fatal(err)
	}
	oversized := referenceRequest(HeartbeatEvent, map[string]any{"prompt": strings.Repeat("x", MaxRequestBytes+1)})
	if _, err := receiver.Export(context.Background(), oversized); status.Code(err) != codes.ResourceExhausted {
		t.Fatal(err)
	}
	duplicate := referenceRequest(HeartbeatEvent, heartbeatFields(now))
	record := duplicate.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	record.Attributes = append(record.Attributes, record.Attributes[0])
	response, err = receiver.Export(context.Background(), duplicate)
	if err != nil || response.GetPartialSuccess().GetRejectedLogRecords() != 1 {
		t.Fatal("duplicate attribute accepted")
	}
	if _, err := receiver.Export(context.Background(), &collectorlogpb.ExportLogsServiceRequest{ResourceLogs: nil}); err != nil {
		t.Fatal(err)
	}
}
func TestTransitionsBoundIncludesRapidIngestedChanges(t *testing.T) {
	now := testTime
	b := backendFixture(t, &now)
	for i := 0; i < MaxTransitions*3; i++ {
		a := heartbeatFields(now)
		a["sequence"] = int64(i + 1)
		if i%2 == 0 {
			a["state"] = "stalled"
			a["reasonCode"] = "no_progress"
		}
		ingest(t, b, HeartbeatEvent, a)
	}
	r := oneReport(t, b)
	if len(r.Transitions) != MaxTransitions {
		t.Fatalf("transitions=%d", len(r.Transitions))
	}
	if r.Transitions[len(r.Transitions)-1].To != "idle" {
		t.Fatal(r.Transitions)
	}
}

func TestReceiverRecordCountLimit(t *testing.T) {
	now := testTime
	b := backendFixture(t, &now)
	receiver, _ := NewReceiver(b, func(context.Context) (string, error) { return "tenant-a", nil })
	req := referenceRequest(HeartbeatEvent, heartbeatFields(now))
	scope := req.ResourceLogs[0].ScopeLogs[0]
	record := scope.LogRecords[0]
	for len(scope.LogRecords) <= MaxRequestRecords {
		scope.LogRecords = append(scope.LogRecords, record)
	}
	if _, err := receiver.Export(context.Background(), req); status.Code(err) != codes.ResourceExhausted {
		t.Fatal(err)
	}
	if !oneReport(t, b).ReceivedAt.IsZero() {
		t.Fatal("oversized request partially mutated inventory")
	}
}

func referenceRequest(name string, attrs map[string]any) *collectorlogpb.ExportLogsServiceRequest {
	record := &logpb.LogRecord{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: name}}}
	for key, value := range attrs {
		v := &commonpb.AnyValue{}
		switch x := value.(type) {
		case string:
			v.Value = &commonpb.AnyValue_StringValue{StringValue: x}
		case int64:
			v.Value = &commonpb.AnyValue_IntValue{IntValue: x}
		case bool:
			v.Value = &commonpb.AnyValue_BoolValue{BoolValue: x}
		default:
			panic("invalid reference scalar")
		}
		record.Attributes = append(record.Attributes, &commonpb.KeyValue{Key: key, Value: v})
	}
	return &collectorlogpb.ExportLogsServiceRequest{ResourceLogs: []*logpb.ResourceLogs{{ScopeLogs: []*logpb.ScopeLogs{{LogRecords: []*logpb.LogRecord{record}}}}}}
}

func TestServiceHealthFilteringDoesNotEstablishLiveness(t *testing.T) {
	now := testTime
	b := backendFixture(t, &now)
	receiver, _ := NewReceiver(b, func(context.Context) (string, error) { return "tenant-a", nil })
	req := referenceRequest("goobers.service.health", map[string]any{"instanceId": "instance"})
	response, err := receiver.Export(context.Background(), req)
	if err != nil || response.GetPartialSuccess().GetRejectedLogRecords() != 0 {
		t.Fatalf("%v %v", response, err)
	}
	if r := oneReport(t, b); !r.ReceivedAt.IsZero() {
		t.Fatal("service health established fleet liveness")
	}
}
