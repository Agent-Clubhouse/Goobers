package schemas

import "testing"

func TestInstanceSchemaNamedExporters(t *testing.T) {
	schema := compileInstanceSchema(t)
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{`telemetry: {exporters: [{name: collector, kind: otlp-grpc, endpoint: 'https://collector.example:4317', headers: {authorization: {env: TOKEN}}, tls: {caFile: ca.pem}}, {name: tenant, kind: azuremonitor, connection: {env: CONNECTION}}]}`, true},
		{`telemetry: {exporters: [{name: bad, kind: otlp-http, endpoint: 'https://collector.test'}]}`, false},
		{`telemetry: {exporters: [{name: bad, kind: azuremonitor}]}`, false},
		{`telemetry: {exporters: [{name: bad, kind: azuremonitor, connection: {value: secret}}]}`, false},
		{`telemetry: {exporters: [{name: bad, kind: azuremonitor, endpoint: 'https://collector.test', connection: {env: CONNECTION}}]}`, false},
		{`telemetry: {exporters: [{name: bad, kind: otlp-grpc, endpoint: 'https://collector.test', replay: {maxAge: 24h}}]}`, false},
		{`telemetry: {otlp: {endpoint: 'https://legacy.test'}, exporters: [{name: collector, kind: otlp-grpc, endpoint: 'https://collector.test'}]}`, false},
	} {
		err := validateInstanceYAML(t, schema, "apiVersion: goobers.dev/v1alpha1\nkind: Instance\nrepos: []\n"+tc.body)
		if (err == nil) != tc.valid {
			t.Errorf("valid=%t error=%v: %s", tc.valid, err, tc.body)
		}
	}
}
