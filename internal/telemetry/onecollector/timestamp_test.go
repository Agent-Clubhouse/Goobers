package onecollector

import "testing"

func TestExplicitTimestampFormats(t *testing.T) {
	for _, test := range []struct{ format, input, want string }{
		{"rfc3339", "2026-01-02T03:04:05.123456700Z", "2026-01-02T03:04:05.123456700Z"},
		{"rfc3339_millis", "2026-01-02T03:04:05.123999999+01:00", "2026-01-02T02:04:05.123Z"},
		{"unix_nano_millis", "1000000001", "1970-01-01T00:00:01.000Z"},
		{"unix_nano", "1000000001", "1970-01-01T00:00:01.000000001Z"},
	} {
		t.Run(test.format, func(t *testing.T) {
			m := Mapping{Signal: "input", EventName: "Synthetic.Time", EnvelopeVersion: "4.0", TimestampSource: "input.time", TimestampFormat: test.format, DataSource: "input"}
			envelope, err := m.Envelope(Facts{"input.time": JSON(test.input), "input": JSON(map[string]any{})}, "", "")
			if err != nil || envelope.Time != test.want || !envelope.PreserveInput {
				t.Fatal(envelope, err)
			}
		})
	}
}
