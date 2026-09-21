package fleetdiagnostics

import (
	"context"
	"errors"

	collectorlogpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const (
	// MaxRequestBytes bounds one decoded reference collector request.
	MaxRequestBytes = 1 << 20
	// MaxRequestRecords bounds work per collector call.
	MaxRequestRecords = 128
)

// Authorizer must derive tenant identity from company-controlled authentication,
// such as verified collector credentials, never from the exported organization.
type Authorizer func(context.Context) (string, error)

// Receiver is an executable OTLP/gRPC Logs consumer. It has no listening socket
// or default endpoint: the caller registers it on its explicitly configured
// server, supplies transport security/authentication and a receive-size limit.
type Receiver struct {
	collectorlogpb.UnimplementedLogsServiceServer
	backend   *Backend
	authorize Authorizer
}

// NewReceiver refuses an unauthenticated or unconfigured collector.
func NewReceiver(backend *Backend, authorize Authorizer) (*Receiver, error) {
	if backend == nil || authorize == nil {
		return nil, errors.New("backend and tenant authorizer required")
	}
	return &Receiver{backend: backend, authorize: authorize}, nil
}

// Export routes every record through authenticated enrollment and the strict
// decoder. Partial rejection reports counts, never rejected private payloads.
func (r *Receiver) Export(ctx context.Context, req *collectorlogpb.ExportLogsServiceRequest) (*collectorlogpb.ExportLogsServiceResponse, error) {
	tenant, err := r.authorize(ctx)
	if err != nil || tenant == "" {
		return nil, status.Error(codes.Unauthenticated, "collector authentication required")
	}
	if req == nil || proto.Size(req) > MaxRequestBytes {
		return nil, status.Error(codes.ResourceExhausted, "diagnostic request too large")
	}
	total := 0
	for _, resource := range req.ResourceLogs {
		for _, scope := range resource.GetScopeLogs() {
			total += len(scope.GetLogRecords())
		}
	}
	if total > MaxRequestRecords {
		return nil, status.Error(codes.ResourceExhausted, "too many diagnostic records")
	}
	var rejected int64
	for _, resource := range req.ResourceLogs {
		for _, scope := range resource.GetScopeLogs() {
			for _, record := range scope.GetLogRecords() {
				if record == nil || proto.Size(record) > 64<<10 {
					rejected++
					continue
				}
				attrs, err := scalarFields(record.Attributes)
				if err == nil {
					_, err = r.backend.Ingest(tenant, record.GetBody().GetStringValue(), attrs)
				}
				if err != nil {
					rejected++
				}
			}
		}
	}
	response := &collectorlogpb.ExportLogsServiceResponse{}
	if rejected > 0 {
		response.PartialSuccess = &collectorlogpb.ExportLogsPartialSuccess{RejectedLogRecords: rejected, ErrorMessage: "records rejected by diagnostic contract or authenticated enrollment"}
	}
	return response, nil
}

func scalarFields(attributes []*commonpb.KeyValue) (map[string]any, error) {
	if len(attributes) > 48 {
		return nil, errors.New("too many attributes")
	}
	attrs := make(map[string]any, len(attributes))
	for _, field := range attributes {
		if field == nil || field.Value == nil {
			return nil, errors.New("missing attribute")
		}
		if _, duplicate := attrs[field.Key]; duplicate {
			return nil, errors.New("duplicate attribute")
		}
		switch value := field.Value.Value.(type) {
		case *commonpb.AnyValue_StringValue:
			attrs[field.Key] = value.StringValue
		case *commonpb.AnyValue_IntValue:
			attrs[field.Key] = value.IntValue
		case *commonpb.AnyValue_DoubleValue:
			attrs[field.Key] = value.DoubleValue
		case *commonpb.AnyValue_BoolValue:
			attrs[field.Key] = value.BoolValue
		default:
			return nil, errors.New("non-scalar attribute")
		}
	}
	return attrs, nil
}
